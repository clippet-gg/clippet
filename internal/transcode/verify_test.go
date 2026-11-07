package transcode

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// box renders one MP4 box with a 32-bit size (payload bytes appended).
func box(typ string, payload int) []byte {
	b := make([]byte, 8+payload)
	binary.BigEndian.PutUint32(b[:4], uint32(8+payload))
	copy(b[4:8], typ)
	return b
}

// largeBox renders a box with size==1 and a 64-bit largesize.
func largeBox(typ string, payload int) []byte {
	b := make([]byte, 16+payload)
	binary.BigEndian.PutUint32(b[:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(16+payload))
	return b
}

// eofBox renders a box with size==0 (extends to end of file).
func eofBox(typ string, payload int) []byte {
	b := make([]byte, 8+payload)
	copy(b[4:8], typ)
	return b
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestVerifyBoxes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string // "" = pass, else substring of the bad_output reason
	}{
		{"faststart", cat(box("ftyp", 16), box("moov", 100), box("mdat", 50)), ""},
		{"free between", cat(box("ftyp", 16), box("free", 8), box("moov", 40), box("mdat", 0)), ""},
		{"largesize mdat", cat(box("ftyp", 16), box("moov", 20), largeBox("mdat", 30)), ""},
		{"largesize moov", cat(box("ftyp", 16), largeBox("moov", 20), box("mdat", 30)), ""},
		{"size 0 mdat to EOF", cat(box("ftyp", 16), box("moov", 20), eofBox("mdat", 1000)), ""},
		{"mdat first", cat(box("ftyp", 16), box("mdat", 50), box("moov", 100)), "moov (82) after mdat (24)"},
		{"no ftyp", cat(box("moov", 10), box("mdat", 10)), "first box is \"moov\""},
		{"no moov", cat(box("ftyp", 16), box("mdat", 10)), "no moov"},
		{"no mdat", cat(box("ftyp", 16), box("moov", 10)), "no mdat"},
		{"truncated", cat(box("ftyp", 16), box("moov", 10))[:20], "overruns"},
		{"tiny size", cat(box("ftyp", 16), []byte{0, 0, 0, 4, 'm', 'o', 'o', 'v'}), "malformed"},
		{"binary type", cat(box("ftyp", 16), []byte{0, 0, 0, 8, 1, 2, 3, 4}), "malformed"},
		{"empty", nil, "no moov"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifyBoxes(bytes.NewReader(c.data), int64(len(c.data)))
			if c.want == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if !IsPermanent(err) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want permanent containing %q", err, c.want)
			}
		})
	}
	// The file-based entry point.
	path := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(path, cat(box("ftyp", 16), box("moov", 8), box("mdat", 8)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMP4(path); err != nil {
		t.Fatalf("VerifyMP4: %v", err)
	}
	if err := VerifyMP4(filepath.Join(t.TempDir(), "missing.mp4")); err == nil || IsPermanent(err) {
		t.Fatalf("missing file must be a plain error: %v", err)
	}
}

func TestParseOutputFixture(t *testing.T) {
	info, err := ParseOutput(fixture(t, "output-720p30.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := OutputInfo{Width: 1280, Height: 720, FPS: Rational{30, 1}, DurationMS: 2020, HasAudio: true,
		VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1,
		// The container says 2.020 s; the PICTURE stops at 2.000 (§20 erratum 92).
		VideoEndSec: 2}
	if *info != want {
		t.Fatalf("ParseOutput = %+v, want %+v", *info, want)
	}
	if err := CheckOutput(info, PlanOptions{MaxW: 1280, MaxH: 720}); err != nil {
		t.Fatalf("CheckOutput on a good output: %v", err)
	}
	if _, err := ParseOutput([]byte("{")); !IsPermanent(err) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestCheckOutput(t *testing.T) {
	good := func() *OutputInfo {
		return &OutputInfo{Width: 1280, Height: 720, FPS: Rational{30, 1}, DurationMS: 6000, HasAudio: true,
			VideoCodec: "h264", AudioCodec: "aac", VideoStreams: 1, AudioStreams: 1}
	}
	box := PlanOptions{MaxW: 1280, MaxH: 720, MaxFPS: 60}
	cases := []struct {
		name string
		mut  func(*OutputInfo)
		want string
	}{
		{"ok", func(*OutputInfo) {}, ""},
		{"no audio ok", func(o *OutputInfo) { o.AudioStreams, o.HasAudio, o.AudioCodec = 0, false, "" }, ""},
		{"vertical ok", func(o *OutputInfo) { o.Width, o.Height = 720, 1280 }, ""},
		{"two video", func(o *OutputInfo) { o.VideoStreams = 2 }, "exactly one video"},
		{"no video", func(o *OutputInfo) { o.VideoStreams = 0 }, "exactly one video"},
		{"hevc", func(o *OutputInfo) { o.VideoCodec = "hevc" }, "want h264"},
		{"two audio", func(o *OutputInfo) { o.AudioStreams = 2 }, "at most one audio"},
		{"opus", func(o *OutputInfo) { o.AudioCodec = "opus" }, "want aac"},
		{"odd width", func(o *OutputInfo) { o.Width = 1279 }, "not even"},
		{"zero height", func(o *OutputInfo) { o.Height = 0 }, "not even"},
		{"too wide", func(o *OutputInfo) { o.Width = 1282 }, "exceed"},
		{"short side too big", func(o *OutputInfo) { o.Width, o.Height = 1280, 722 }, "exceed"},
		{"no duration", func(o *OutputInfo) { o.DurationMS = 0 }, "duration"},
		{"bad fps", func(o *OutputInfo) { o.FPS = Rational{0, 0} }, "frame rate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := good()
			c.mut(o)
			err := CheckOutput(o, box)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if !IsPermanent(err) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want permanent containing %q", err, c.want)
			}
		})
	}
	if err := CheckOutput(nil, box); !IsPermanent(err) {
		t.Fatalf("nil info: %v", err)
	}
}
