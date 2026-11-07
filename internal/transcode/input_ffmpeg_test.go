//go:build ffmpeg

package transcode

// The ingest gate, proven against the real ffprobe: a crafted playlist is
// refused whether it points OUT of the machine or at a sibling file the
// uploader was never entitled to (§20 erratum 91). The two halves fail
// through different doors — the protocol whitelist stops the first before a
// byte is fetched, the container allowlist stops the second after ffprobe
// happily reported a video stream — and both land on the same permanent
// verdict the upload gate answers 415 not_a_video with.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeRefusesACraftedPlaylist(t *testing.T) {
	ffmpeg, ffprobe := binaries(t)
	dir := t.TempDir()

	// A real, playable sibling: the file the second playlist tries to reach
	// from a directory of its own, exactly as one clip's scratch directory
	// sits beside another's.
	victim := filepath.Join(dir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=1:size=320x180:rate=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		filepath.Join(victim, "source.mp4"))

	attacker := filepath.Join(dir, "attacker")
	if err := os.MkdirAll(attacker, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		t.Helper()
		// source.m3u8 is what the spool names it: store.SanitizeExt lets the
		// uploader choose the extension, which is why the gate cannot rely
		// on the name.
		path := filepath.Join(attacker, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const head = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.000000,\n"
	// Port 1 is never listening: if the whitelist ever stops holding, this
	// fails as a connection error rather than a protocol refusal, and the
	// test still fails — but it must never SUCCEED.
	egress := write("source.m3u8", head+"http://127.0.0.1:1/x.ts\n#EXT-X-ENDLIST\n")
	// A BARE relative reference, not `file:../victim/source.mp4`: ffmpeg joins
	// a scheme-qualified relative segment URL against the playlist's own
	// absolute path into something that opens nothing, so the bare form is the
	// one that actually reaches the sibling — the harder case to refuse.
	sibling := write("sibling.m3u8", head+"../victim/source.mp4\n#EXT-X-ENDLIST\n")

	probe := Probe(ffprobe)
	for _, c := range []struct{ name, path string }{
		{"an http segment", egress},
		{"a sibling clip's source", sibling},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := probe(context.Background(), c.path)
			if err == nil {
				t.Fatalf("the playlist was accepted as %dx%d %s", p.CodedW, p.CodedH, p.FormatName)
			}
			if !IsPermanent(err) {
				t.Fatalf("err = %v, want a permanent (file-at-fault) refusal", err)
			}
		})
	}
	// The sibling half is the allowlist's: ffprobe reads the local file
	// perfectly well, reports "hls", and only the container rule says no.
	if _, err := probe(context.Background(), sibling); !strings.Contains(err.Error(), "not_a_video: unsupported container") {
		t.Fatalf("sibling: err = %v, want the container allowlist to be what refuses it", err)
	}
	// A plain file in the same directory still probes: the whitelist keeps
	// `file` because that is the whole of what the pipeline reads.
	if _, err := probe(context.Background(), filepath.Join(victim, "source.mp4")); err != nil {
		t.Fatalf("an ordinary local file must still probe: %v", err)
	}
}
