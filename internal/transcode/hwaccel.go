package transcode

import (
	"strconv"
	"strings"
)

// The GPU fast path (DESIGN.md §20 erratum 216).
//
// A 2026-09-08 investigation measured, on the host this was built on (RTX 5090,
// Docker Desktop/WSL2): a full CUDA path — `-hwaccel cuda
// -hwaccel_output_format cuda`, `scale_cuda`, `h264_nvenc` — is 9–11× faster
// than libx264 AT ONE CPU and is nearly core-count independent, so a 1-vCPU box
// with a card beats a sixteen-core box without one. Swapping only the encoder
// (CPU decode, CPU scale, nvenc) is 1.4–3.6×, because the decode and the
// lanczos are ~82 % of that pipeline; it is not worth a second image.
//
// So this is the FULL path or nothing, and it is OPT-IN: `TRANSCODE_HWACCEL=cuda`
// on the `gpu` image. Two facts make it a config value read by the PLAN rather
// than a build flag:
//
//   - the app's own static ffmpeg (`mwader/static-ffmpeg:7.1.1`) has ZERO nvenc
//     encoders, so the CPU image cannot use it however it is asked;
//   - a CUDA graph cannot render most of what this pipeline's filter chain does.
//     `yadif`, `zscale`/`tonemap`, `transpose` and `setsar` are CPU filters, and
//     `setparams` is too — so the interlaced, HDR, tag-rotated and anamorphic
//     sources errata 93/125/200 exist for MUST fall back to the CPU argv. A fast
//     path with a fallback, never a replacement.
//
// And one trap, isolated by bisection during that investigation: `-colorspace
// bt709` on the OUTPUT cannot survive a CUDA graph. It is the one of the three
// colour options ffmpeg 7 actually honours, so it forces buffersink colorspace
// negotiation, ffmpeg inserts a CPU `auto_scale` between `scale_cuda` and the
// sink, and the run dies — "Impossible to convert between the formats supported
// by the filter 'Parsed_scale_cuda_0' and the filter 'auto_scale_0'", exit 218,
// zero-byte output. The free replacement is the `h264_metadata` bitstream
// filter, which writes the same three values into the SPS VUI where a decoder
// reads them.

// HWAccel values. The empty string is the CPU argv this package has always
// rendered; config.HWAccelNone is the same value on the configuration side.
const (
	HWAccelNone = ""
	HWAccelCUDA = "cuda"

	// hwAccelNVENCOnly is the RUN-TIME fallback of §20 erratum 265: the decode
	// and every filter on the CPU, the encoder still on the card. It is NOT a
	// configured value (config.hwaccel accepts "" and "cuda" and nothing else)
	// and NOT a plan decision — HWAccelFor never returns it. It exists for one
	// transition: a CUDA run whose ffmpeg died because NVDEC refused the source
	// at hwaccel initialisation, which is a fact about a stream that no probe
	// can predict and only a run can discover.
	hwAccelNVENCOnly = "nvenc"
)

// cudaGraph reports whether accel keeps the frames on the card: the `-hwaccel
// cuda` input options, the one-filter `scale_cuda` graph, and no CPU filter
// anywhere in it.
func cudaGraph(accel string) bool { return accel == HWAccelCUDA }

// nvencOut reports whether accel encodes with NVENC — true for the full CUDA
// path and for the fallback that kept only the encoder on the card, and it is
// the whole of the difference between them on the OUTPUT side.
func nvencOut(accel string) bool { return accel == HWAccelCUDA || accel == hwAccelNVENCOnly }

// The NVENC encoder settings (§20 erratum 216).
//
// `p4` is NVENC's own middle preset and `-tune hq` the quality tuning; `-rc vbr
// -cq N -b:v 0` is NVENC's constant-quality mode, which is the closest thing it
// has to x264's CRF — `-b:v 0` is what stops it from also honouring an average
// bitrate and turning `cq` into a suggestion.
const (
	NVENCH264 = "h264_nvenc"
	// NVENCPreset and NVENCTune: p4/hq is the balance point. p7 is slower than
	// x264 veryfast on this hardware for a fraction of a VMAF point.
	NVENCPreset = "p4"
	NVENCTune   = "hq"
	// The offsets that have to be ADDED to an x264 CRF to land near the same
	// BYTES. NVENC's quantizer scale is not x264's — at the same number the
	// measured output is 1.8–4× the size — and the offset is not one number
	// either, which is why there are two of these and not a constant.
	//
	// MEASURED on this host's ffmpeg with `h264_nvenc -preset p4 -tune hq -rc vbr
	// -cq N -b:v 0`, against the investigation's 60 s 1920×1080 60 fps sample
	// (§20 erratum 216 carries the whole table):
	//
	//	FULL  (x264 CRF 18 → 238.7 MB): cq 22 → 232.9 MB, −2.4 %  ✓
	//	                                cq 24 → 183.0 MB, −23 %
	//	LIGHT (x264 CRF 26 → 10.06 MB): cq 34 → 11.38 MB, +13.2 % ✓
	//	                                cq 32 → 14.64 MB, +45.6 %
	//
	// So +4 for the Full and +8 for the Light copy, each the value that lands
	// inside ±15 % of the x264 file it replaces.
	NVENCCQOffsetFull  = 4
	NVENCCQOffsetLight = 8
	// NVENCCQOffsetDefault is what an encode that named no tier gets — a trim's
	// re-encode at TRANSCODE_CRF, which sits between the two. It is also what
	// CQOffset = 0 means, because zero is not a plausible offset.
	NVENCCQOffsetDefault = 6
	// NVENCMaxCQ is the encoder's own ceiling.
	NVENCMaxCQ = 51
)

// h264MetadataBSF is the Rec.709 stamp of a CUDA encode: primaries 1, transfer
// 1, matrix 1 are BT.709's own code points in ISO/IEC 23001-8, written into the
// SPS VUI by a bitstream filter that touches no pixel. It replaces BOTH the
// `-colorspace`/`-color_primaries`/`-color_trc` triple (which kills a CUDA
// graph, see above) and the `setparams` link of colourStamp (a CPU filter).
const h264MetadataBSF = "h264_metadata=colour_primaries=1:transfer_characteristics=1:matrix_coefficients=1"

// Why a plan cannot take the CUDA path. They are operator text — a boot/plan
// log field — and each names the FILTER that is CPU-only, because that is the
// fact an operator can check against `probe.json`.
const (
	hwWhyOff         = "not configured"
	hwWhyTonemap     = "the source is HDR and the tone map is a CPU filter"
	hwWhyDeinterlace = "the source is interlaced and yadif is a CPU filter"
	hwWhyRotate      = "the source needs a pre-rotate and transpose is a CPU filter"
	hwWhyAnamorphic  = "the source is anamorphic and setsar is a CPU filter"
	hwWhyMirror      = "the source's display matrix mirrors the picture and only the CPU graph's autorotate reproduces it"
	hwWhyCodec       = "av1_nvenc is not built into this image"
	hwWhyDecoder     = "the source's codec or pixel format has no hardware decoder (NVDEC takes 8-bit 4:2:0 H.264, HEVC, VP8, VP9, AV1, MPEG-2, MPEG-4, MJPEG, VC-1)"
)

// nvdecCodecs are the ffmpeg codec names NVDEC decodes; nvdecPixFmts the
// 8-bit 4:2:0 layouts every one of them takes (§20 erratum 247). Anything
// else — a 10-bit High 10 H.264 from a capture card, ProRes, DNxHD, FFV1,
// 4:4:4 — makes `-hwaccel cuda` fall back to a SOFTWARE decode without an
// error, after which `scale_cuda` cannot reach the frames and the run dies
// with a zero-byte file (proven in the gpu image on a High 10 source).
// Refusing here keeps such a source on the CPU argv, which handles it. A
// 10-bit HEVC/VP9/AV1 source COULD take the card with a format conversion
// on the device; that is not built (a known, deferred gap).
var nvdecCodecs = map[string]bool{
	"h264": true, "hevc": true, "vp8": true, "vp9": true, "av1": true,
	"mpeg1video": true, "mpeg2video": true, "mpeg4": true, "mjpeg": true, "vc1": true,
}

var nvdecPixFmts = map[string]bool{"yuv420p": true, "yuvj420p": true, "nv12": true}

// hwDecodable reports whether NVDEC can decode the plan's source. Empty
// fields (a plan built without a probe) are unknown, not refused.
func hwDecodable(p Plan) bool {
	if p.SourceCodec != "" && !nvdecCodecs[strings.ToLower(p.SourceCodec)] {
		return false
	}
	if p.SourcePixFmt != "" && !nvdecPixFmts[strings.ToLower(p.SourcePixFmt)] {
		return false
	}
	return true
}

// HWAccelFor decides whether one encode takes the hardware path, and says why
// when it does not.
//
// accel is "" (the CPU argv) or HWAccelCUDA; why is empty exactly when accel is
// HWAccelCUDA, and is the operator sentence otherwise. The worker logs the pair
// on its `planned` line and EncodeArgs asks the same function, so the argv and
// the log can never disagree.
func HWAccelFor(p Plan, configured, codec string) (accel, why string) {
	if !strings.EqualFold(strings.TrimSpace(configured), HWAccelCUDA) {
		return HWAccelNone, hwWhyOff
	}
	switch {
	case NormalizeFullCodec(codec) == FullCodecAV1:
		// FULL_CODEC=av1 stays on libsvtav1: `av1_nvenc` is fast but no card
		// platform plays AV1 and its bytes at a comparable quality were worse,
		// so the investigation's verdict was "do not build".
		return HWAccelNone, hwWhyCodec
	case p.Tonemap:
		return HWAccelNone, hwWhyTonemap
	case p.Deinterlace:
		return HWAccelNone, hwWhyDeinterlace
	case p.PreRotate != 0:
		return HWAccelNone, hwWhyRotate
	case p.Anamorphic:
		return HWAccelNone, hwWhyAnamorphic
	case p.MatrixMirrored:
		return HWAccelNone, hwWhyMirror
	case !hwDecodable(p):
		return HWAccelNone, hwWhyDecoder
	}
	return HWAccelCUDA, ""
}

// NVENCCQ is the `-cq` for an encode whose x264 CRF is crf, offset by the tier's
// own calibration (0 = NVENCCQOffsetDefault).
func NVENCCQ(crf, offset int) int {
	if offset == 0 {
		offset = NVENCCQOffsetDefault
	}
	cq := crf + offset
	if cq > NVENCMaxCQ {
		return NVENCMaxCQ
	}
	if cq < 0 {
		return 0
	}
	return cq
}

// cudaInputArgs are the options that must precede `-i` for the decode to land
// in device memory. Without `-hwaccel_output_format cuda` ffmpeg copies every
// frame back to system memory and the scale runs on the CPU, which is the tier
// the investigation measured as not worth an image.
func cudaInputArgs() []string {
	return []string{"-hwaccel", HWAccelCUDA, "-hwaccel_output_format", HWAccelCUDA}
}

// cudaScaleFilter is the whole video work of a CUDA encode: one filter, with no
// input or output pad.
//
// `scale_cuda` does the resize on the card and converts to the encoder's pixel
// format in the same pass. There is deliberately no `setsar` (the plan is only
// here when the source is square-pixel), no `format` filter (scale_cuda's own
// `format` option does it) and no `setparams` (a CPU filter — the colour stamp
// is the h264_metadata bitstream filter instead).
func cudaScaleFilter(p Plan) string { return cudaScaleTo(p.W, p.H) }

// cudaScaleTo is that filter for an explicit frame size: a matrix-turned source
// scales to its CODED orientation before the turn (Plan.cudaBranch, §20
// erratum 352).
func cudaScaleTo(w, h int) string {
	return "scale_cuda=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) +
		":interp_algo=lanczos:format=yuv420p"
}

// nvencCodecArgs are the encoder options of a CUDA output, in place of
// videoCodecArgs' libx264 line.
func nvencCodecArgs(crf, cqOffset int) []string {
	return []string{"-c:v", NVENCH264, "-preset", NVENCPreset, "-tune", NVENCTune,
		"-rc", "vbr", "-cq", strconv.Itoa(NVENCCQ(crf, cqOffset)), "-b:v", "0"}
}
