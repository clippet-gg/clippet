package worker

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"clippet/internal/db"
)

func TestRegistryRateLimitAndRetention(t *testing.T) {
	c := &clock{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}
	r := newRegistry(c.Now)

	r.set("a", Progress{State: db.ClipProcessing, Stage: stageEncode, Percent: 10}, true)
	r.set("a", Progress{State: db.ClipProcessing, Stage: stageEncode, Percent: 11}, false) // < 250ms: dropped
	if p, _ := r.get("a"); p.Percent != 10 {
		t.Fatalf("rate limit: percent = %d, want 10", p.Percent)
	}
	c.Advance(registryMinInterval)
	r.set("a", Progress{State: db.ClipProcessing, Stage: stageEncode, Percent: 12}, false)
	if p, _ := r.get("a"); p.Percent != 12 || !p.UpdatedAt.Equal(c.Now()) {
		t.Fatalf("after interval: %+v", p)
	}
	r.set("a", Progress{State: db.ClipProcessing, Stage: stageVerify, Percent: 85}, true) // forced lands at once
	if p, _ := r.get("a"); p.Stage != stageVerify {
		t.Fatalf("forced write dropped: %+v", p)
	}

	// Terminal entries survive 10 minutes, then vanish (on read and on the
	// next write's purge).
	r.set("a", Progress{State: db.ClipReady, Stage: stageDone, Percent: 100}, true)
	c.Advance(registryTerminalKeep - time.Second)
	if _, ok := r.get("a"); !ok {
		t.Fatal("terminal entry evicted too early")
	}
	c.Advance(2 * time.Second)
	if _, ok := r.get("a"); ok {
		t.Fatal("terminal entry not evicted after 10 minutes")
	}
	r.set("b", Progress{State: db.ClipFailed, Error: "x"}, true)
	c.Advance(registryTerminalKeep)
	r.set("c", Progress{State: db.ClipProcessing}, true)
	if _, ok := r.get("b"); ok || r.len() != 1 {
		t.Fatalf("purge on write: len=%d", r.len())
	}
	// Non-terminal entries are not aged out.
	c.Advance(24 * time.Hour)
	if _, ok := r.get("c"); !ok {
		t.Fatal("processing entry must not expire")
	}
	r.forget("c")
	if _, ok := r.get("c"); ok {
		t.Fatal("forget")
	}
}

// TestRegistryAwait covers the CAPTURE_READY_WAIT wake-up (§20 erratum 97):
// a waiter fires on the terminal write and only on it, a clip that is already
// terminal hands back a closed channel (the race POST /api/upload cannot close
// itself), the deregister leaves nothing behind, and every waiter on one clip
// is woken.
func TestRegistryAwait(t *testing.T) {
	c := &clock{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}
	r := newRegistry(c.Now)

	closed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	// A clip with no entry at all: the upload registers before the worker has
	// claimed the job, which is the normal case.
	one, stopOne := r.await("a")
	two, stopTwo := r.await("a")
	other, stopOther := r.await("b")
	if closed(one) || closed(two) || closed(other) {
		t.Fatal("a waiter fired before anything happened")
	}
	c.Advance(time.Second)
	r.set("a", Progress{State: db.ClipProcessing, Stage: stageEncode, Percent: 40}, true)
	if closed(one) || closed(two) {
		t.Fatal("a progress line woke a waiter; only a terminal state may")
	}
	c.Advance(time.Second)
	r.set("a", Progress{State: db.ClipReady, Stage: stageDone, Percent: 100}, true)
	if !closed(one) || !closed(two) {
		t.Fatal("the terminal write did not wake every waiter on the clip")
	}
	if closed(other) {
		t.Fatal("another clip's terminal write woke this waiter")
	}
	stopOne()
	stopTwo()

	// Already terminal → closed at once, and `failed` counts too.
	if ch, stop := r.await("a"); !closed(ch) {
		stop()
		t.Fatal("await on a clip that is already ready must come back closed")
	}
	c.Advance(time.Second)
	r.set("b", Progress{State: db.ClipFailed, Stage: stageEncode, Error: "boom"}, true)
	if !closed(other) {
		t.Fatal("a failed clip must end the wait too")
	}
	stopOther()

	// The deregister is the whole leak story: a waiter for a clip that never
	// finishes must leave no map entry behind.
	_, stopNever := r.await("never")
	if len(r.waiting) != 1 {
		t.Fatalf("waiting map = %d entries, want 1", len(r.waiting))
	}
	stopNever()
	stopNever() // idempotent: the wait's defer may run after a wake
	if len(r.waiting) != 0 {
		t.Fatalf("waiting map = %d entries after the deregister, want 0", len(r.waiting))
	}
}

func TestRegistryCap(t *testing.T) {
	c := &clock{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}
	r := newRegistry(c.Now)
	for i := 0; i < registryCap+5; i++ {
		c.Advance(time.Millisecond)
		r.set(fmt.Sprintf("clip%04d", i), Progress{State: db.ClipProcessing, Percent: i}, true)
	}
	if r.len() != registryCap {
		t.Fatalf("len = %d, want %d", r.len(), registryCap)
	}
	for i := 0; i < 5; i++ {
		if _, ok := r.get(fmt.Sprintf("clip%04d", i)); ok {
			t.Fatalf("oldest entry clip%04d survived the cap", i)
		}
	}
	if _, ok := r.get(fmt.Sprintf("clip%04d", registryCap+4)); !ok {
		t.Fatal("newest entry evicted")
	}
}

// OwnerError is erratum 165's rule in one function: a kind with a closed
// vocabulary answers with its own sentence when it recognises the text and
// with its own FALLBACK when it does not, so an operator's wording can never
// reach a person who is not one; a kind without a closed set (the gif, whose
// last_error is publicReason's redacted "stage: message") passes its own text
// through and drops only the one string db itself writes.
func TestOwnerErrorSpeaksEachKindsOwnVocabulary(t *testing.T) {
	for name, tc := range map[string]struct {
		kind, last, want string
	}{
		"poster keeps its own sentence":  {db.KindPoster, msgNoFrame, msgNoFrame},
		"poster keeps its disk sentence": {db.KindPoster, msgFrameDisk, msgFrameDisk},
		"poster on a restart":            {db.KindPoster, db.MsgInterruptedByRestart, msgFrameInternal},
		"poster on an unstick":           {db.KindPoster, db.UnstickReason, msgFrameInternal},
		"poster on the trim's words":     {db.KindPoster, msgInternal, msgFrameInternal},
		"poster on anything else":        {db.KindPoster, "ffmpeg exited 1 in /scratch/abc", msgFrameInternal},
		"poster with nothing to say":     {db.KindPoster, "", ""},
		"trim keeps its own sentence":    {db.KindTrim, msgNoWindow, msgNoWindow},
		"trim on a restart":              {db.KindTrim, db.MsgInterruptedByRestart, msgInternal},
		"trim on the poster's words":     {db.KindTrim, msgFrameInternal, msgInternal},
		"replace keeps its own sentence": {db.KindReplace, msgReplaceTooLarge, msgReplaceTooLarge},
		"replace on a restart":           {db.KindReplace, db.MsgInterruptedByRestart, msgReplaceInternal},
		"gif keeps the worker's text":    {db.KindGIF, "encode: ffmpeg exited 1", "encode: ffmpeg exited 1"},
		"gif on a restart says nothing":  {db.KindGIF, db.MsgInterruptedByRestart, ""},
		"gif on an unstick says nothing": {db.KindGIF, db.UnstickReason, ""},
		"gif with nothing to say":        {db.KindGIF, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := OwnerError(tc.kind, tc.last); got != tc.want {
				t.Fatalf("OwnerError(%q, %q) = %q, want %q", tc.kind, tc.last, got, tc.want)
			}
		})
	}
	// Neither sentence internal/db writes may reach ANY of these surfaces,
	// whatever kind carries it — that is the whole class this closes, and
	// db.IsOperatorReason is where a third one would be added.
	for _, operator := range []string{db.MsgInterruptedByRestart, db.UnstickReason} {
		if !db.IsOperatorReason(operator) {
			t.Fatalf("db.IsOperatorReason does not recognise its own %q", operator)
		}
		for _, kind := range []string{db.KindPoster, db.KindTrim, db.KindReplace, db.KindGIF} {
			if got := OwnerError(kind, operator); got == operator {
				t.Errorf("%s passed an operator's sentence through: %q", kind, got)
			}
		}
	}

	// Every sentence in every closed set survives its own kind: the mapping
	// must never rewrite a text the worker itself wrote.
	for kind, vocab := range ownerVocabulary {
		for _, sentence := range vocab {
			if got := OwnerError(kind, sentence); got != sentence {
				t.Errorf("OwnerError(%q, %q) rewrote a sentence of that kind's own vocabulary as %q", kind, sentence, got)
			}
		}
		if ownerFallback[kind] == "" {
			t.Errorf("kind %q has a closed vocabulary and no fallback sentence", kind)
		}
		if !slices.Contains(vocab, ownerFallback[kind]) {
			t.Errorf("kind %q falls back to %q, which is not in its own vocabulary", kind, ownerFallback[kind])
		}
	}

	// Every job kind is accounted for by NAME, so that "" is read as the
	// decision it is rather than as a key somebody forgot: a kind whose
	// endings can reach a person who is not an operator must have an entry
	// (empty or not), and the three kinds that never do must not have one.
	for kind, owner := range map[string]bool{
		db.KindPoster:    true,
		db.KindTrim:      true,
		db.KindReplace:   true,
		db.KindGIF:       true,
		db.KindFilmstrip: true,
		db.KindFull:      true,
		db.KindTranscode: false,
		db.KindSocial:    false,
		db.KindNotify:    false,
	} {
		if _, ok := ownerFallback[kind]; ok != owner {
			t.Errorf("ownerFallback[%q] present = %v, want %v", kind, ok, owner)
		}
	}
}
