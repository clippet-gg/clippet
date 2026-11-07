package transcode

import (
	"strings"
	"testing"
)

// §20 erratum 351: the Light copy encoded from a PRODUCED file (the Full, or
// the remux) must address that file's streams — the video first, then its one
// audio track — not the upload's, whose order a phone recording may invert.
func TestForProducedFileAddressesTheProducedLayout(t *testing.T) {
	src := Plan{W: 626, H: 1234, VideoIndex: 1, AudioIndexes: []int{0}, AudioMode: AudioSingle, HasDuration: true, DurationUS: 20028333}
	got := src.ForProducedFile(true)
	if got.VideoIndex != 0 {
		t.Fatalf("video index: got %d, want 0 (the produced file's video is first)", got.VideoIndex)
	}
	if len(got.AudioIndexes) != 1 || got.AudioIndexes[0] != 1 || got.AudioMode != AudioSingle {
		t.Fatalf("audio: got %v/%s, want [1]/single (the produced file's one audio track is second)", got.AudioIndexes, got.AudioMode)
	}
	if got.W != src.W || got.H != src.H || got.DurationUS != src.DurationUS || !got.HasDuration {
		t.Fatalf("the picture's decisions must hold: got %+v", got)
	}
	if src.VideoIndex != 1 || src.AudioIndexes[0] != 0 {
		t.Fatalf("the upload's plan must be untouched: %+v", src)
	}
	// the graph the Light pass renders names stream 0 for the video and 1 for the audio
	fc := got.filterComplex("", LightTag{})
	if !strings.Contains(fc, "[0:0]") || !strings.Contains(fc, "[0:1]aresample") {
		t.Fatalf("filtergraph does not address the produced layout: %s", fc)
	}
	if strings.Contains(src.filterComplex("", LightTag{}), "[0:0]aresample") == false {
		t.Fatalf("the upload's own graph still addresses the upload's layout")
	}
}

// A merge of two upload tracks is ONE track in the produced file: read as Single.
func TestForProducedFileReadsAMergeAsOneTrack(t *testing.T) {
	src := Plan{W: 1280, H: 720, VideoIndex: 0, AudioIndexes: []int{1, 2}, AudioMode: AudioMerge}
	got := src.ForProducedFile(true)
	if got.AudioMode != AudioSingle || len(got.AudioIndexes) != 1 || got.AudioIndexes[0] != 1 {
		t.Fatalf("got %v/%s, want [1]/single", got.AudioIndexes, got.AudioMode)
	}
	none := Plan{W: 1280, H: 720, VideoIndex: 2, AudioMode: AudioNone}.ForProducedFile(true)
	if none.VideoIndex != 0 || none.AudioMode != AudioNone || len(none.AudioIndexes) != 0 {
		t.Fatalf("a silent file: got %+v", none)
	}
}
