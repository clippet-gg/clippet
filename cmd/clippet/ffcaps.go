package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"clippet/internal/config"
	"clippet/internal/transcode"
)

// Boot capability checks (DESIGN.md §20 errata 214–216).
//
// Three settings name something the BINARY has to be able to do, and none of
// them was checked: FULL_CODEC=av1 needs libsvtav1, TRANSCODE_HWACCEL=cuda needs
// h264_nvenc and scale_cuda (which the CPU image has neither of), and
// LIGHT_TAG_TEXT needs the `drawtext` filter and a font. Without these checks the
// first two turn every encode of an unplayable source into a permanent failure —
// an ffmpeg exit whose reason nobody reads until a clip has failed three times —
// and the third does the same to every clip that wants a card copy.
//
// So they are checked once, at boot, from the same `ffmpeg` this process resolved.
// The two ENCODER settings are fatal: the operator asked for a pipeline this
// binary cannot run, and starting anyway means failing clips one at a time. The
// TAG is not: it is a cosmetic line on one artefact, and an image without a font
// must lose the line and keep the encode.

// ffcapsTimeout bounds the two capability listings. They are local process spawns
// that print a few kilobytes; ten seconds is the same budget resolveFFmpeg gives
// `-version`.
const ffcapsTimeout = 10 * time.Second

// ffCaps is what one ffmpeg build can do, as the two listings report it.
type ffCaps struct {
	encoders string
	filters  string
}

// readFFCaps asks the binary what it has. A listing that cannot be read is not
// fatal on its own — the checks below then have nothing to say, and the encode
// itself is still the authority — but it IS worth a line, because a `-encoders`
// that fails is a broken image.
func readFFCaps(ctx context.Context, ffmpeg string) (ffCaps, error) {
	var caps ffCaps
	for _, spec := range []struct {
		arg  string
		into *string
	}{{"-encoders", &caps.encoders}, {"-filters", &caps.filters}} {
		out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", spec.arg).Output()
		if err != nil {
			return caps, fmt.Errorf("ffmpeg %s: %w", spec.arg, err)
		}
		*spec.into = string(out)
	}
	return caps, nil
}

func (c ffCaps) hasEncoder(name string) bool { return strings.Contains(c.encoders, name) }
func (c ffCaps) hasFilter(name string) bool  { return strings.Contains(c.filters, name) }

// checkFFCaps applies the three checks. It returns the resolved tag font (empty =
// no tag) and a fatal error for a configuration this binary cannot honour.
//
// It is a free function taking the caps so the whole table is testable without a
// process: the ONE thing that must not happen here is a check that is wrong about
// what the image has.
func checkFFCaps(cfg config.Config, caps ffCaps, font string, log *slog.Logger) (string, error) {
	if transcode.NormalizeFullCodec(cfg.Transcode.FullCodec) == transcode.FullCodecAV1 && !caps.hasEncoder("libsvtav1") {
		return "", fmt.Errorf("FULL_CODEC=av1 needs the libsvtav1 encoder, and this ffmpeg has none " +
			"(`ffmpeg -encoders | grep av1`); set FULL_CODEC=h264 or use an ffmpeg built with libsvtav1")
	}
	if cfg.Transcode.HWAccel == config.HWAccelCUDA {
		var missing []string
		if !caps.hasEncoder(transcode.NVENCH264) {
			missing = append(missing, transcode.NVENCH264)
		}
		if !caps.hasFilter("scale_cuda") {
			missing = append(missing, "scale_cuda")
		}
		if len(missing) > 0 {
			return "", fmt.Errorf("TRANSCODE_HWACCEL=cuda needs %s, and this ffmpeg has neither the encoder nor "+
				"the filter listed (the CPU image ships a static ffmpeg with no NVENC at all): run the `gpu` "+
				"image (DEPLOY.md §27) or leave TRANSCODE_HWACCEL empty", strings.Join(missing, " and "))
		}
	}
	if cfg.LightTag.Text == "" {
		return "", nil
	}
	switch {
	case !caps.hasFilter("drawtext"):
		log.Warn("LIGHT_TAG_TEXT is set but this ffmpeg has no drawtext filter; the card copy will carry no tag "+
			"(DEPLOY.md §27)", "light_tag", "off")
		return "", nil
	case font == "":
		log.Warn("LIGHT_TAG_TEXT is set but this image ships no font; the card copy will carry no tag "+
			"(DEPLOY.md §27)", "light_tag", "off")
		return "", nil
	}
	return font, nil
}

// resolveFFCaps is checkFFCaps against the real binary and the real filesystem.
// A listing that cannot be read is logged and the checks are skipped, because a
// boot that refuses to start over an unreadable `-encoders` would be a worse
// failure than the one it is guarding against.
func resolveFFCaps(cfg config.Config, log *slog.Logger) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ffcapsTimeout)
	defer cancel()
	caps, err := readFFCaps(ctx, cfg.Transcode.FFmpeg)
	if err != nil {
		log.Warn("could not list this ffmpeg's encoders and filters; the FULL_CODEC / TRANSCODE_HWACCEL / "+
			"LIGHT_TAG_TEXT boot checks are skipped", "err", err)
		return transcode.FindTagFont(), nil
	}
	return checkFFCaps(cfg, caps, transcode.FindTagFont(), log)
}
