package autobot

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Lane `autoskip` (§20 erratum 334, amended 2026-10-19): a poster on the
// server's skip list — a member who routes his uploads to his own box — is
// ignored by the hosted Auto, so the owner's box never fetches his video. Each
// pin fails with the skip check reverted.

const friendPoster = "400000000000000009"

func skipping(mode string, ids ...string) Settings {
	st := Settings{Mode: mode, Seconds: 30}
	if len(ids) > 0 {
		st.Skip = map[string]bool{}
		for _, id := range ids {
			st.Skip[id] = true
		}
	}
	return st
}

// Pin: under Prompt and under Auto a skipped poster gets no reply, no upload
// (no box call), is not acted on (nothing saved), and the only trace is one
// decision marked skip=person.
func TestSkippedPosterIgnored(t *testing.T) {
	for _, mode := range []string{ModePrompt, ModeAuto} {
		store := &countStore{}
		host := oneGuild(skipping(mode, testPoster))
		a, rest, timers := guildAuto(t, host, store, time.Hour)
		obs := &recObs{}
		a.obs = obs
		a.onMessage(context.Background(), videoMessage(testGuild))
		waitIdle(t, a)
		if n := host.uploadCount(); n != 0 {
			t.Fatalf("%s: a skipped poster made %d box calls", mode, n)
		}
		if calls := rest.snapshot(); len(calls) != 0 {
			t.Fatalf("%s: a skipped poster's video reached Discord's REST: %+v", mode, calls)
		}
		if ts := timers.all(); len(ts) != 0 {
			t.Fatalf("%s: a skipped poster armed %d reply timers", mode, len(ts))
		}
		if n := store.count(); n != 0 {
			t.Fatalf("%s: a skipped poster's message was acted on (%d Saves)", mode, n)
		}
		obs.mu.Lock()
		ds := append([]Decision(nil), obs.decisions...)
		obs.mu.Unlock()
		if len(ds) != 1 || ds[0].Skip != "person" || ds[0].Outcome != "ignored" {
			t.Fatalf("%s: decisions %+v, want one with skip=person", mode, ds)
		}
		for _, f := range []string{ds[0].Event, ds[0].Guild, ds[0].Channel, ds[0].Message, ds[0].Mode, ds[0].Outcome, ds[0].Skip} {
			if strings.Contains(f, testPoster) {
				t.Fatalf("%s: the decision carries the poster's id: %+v", mode, ds[0])
			}
		}
	}
}

// Pin: a poster NOT on the list is served as today, under Prompt (the reply)
// and Auto (the upload); and a server whose settings carry no skip list at all
// behaves exactly as before.
func TestUnlistedPosterActs(t *testing.T) {
	for _, st := range []Settings{skipping(ModePrompt, friendPoster), skipping(ModePrompt)} {
		host := oneGuild(st)
		a, rest, timers := guildAuto(t, host, nil, 0)
		a.onMessage(context.Background(), videoMessage(testGuild))
		offered := false
		for _, c := range rest.snapshot() {
			if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/messages") {
				offered = true
			}
		}
		if !offered || len(timers.all()) != 1 {
			t.Fatalf("skip %v: Prompt offered %v with %d timers: %+v", st.Skip, offered, len(timers.all()), rest.snapshot())
		}
	}
	for _, st := range []Settings{skipping(ModeAuto, friendPoster), skipping(ModeAuto)} {
		host := oneGuild(st)
		a, _, _ := guildAuto(t, host, nil, 0)
		a.onMessage(context.Background(), videoMessage(testGuild))
		waitIdle(t, a)
		if n := host.uploadCount(); n != 1 {
			t.Fatalf("skip %v: Auto made %d box calls, want 1", st.Skip, n)
		}
	}
}

// Pin: a refresh of the settings that drops the poster from the list lets
// his next video through — the set is replaced, never merged.
func TestSkipDroppedLetsNextThrough(t *testing.T) {
	host := oneGuild(skipping(ModeAuto, testPoster))
	a, _, _ := guildAuto(t, host, nil, 0)
	a.onMessage(context.Background(), msgWithID(testGuild, "300000000000000061"))
	waitIdle(t, a)
	if host.uploadCount() != 0 {
		t.Fatal("a skipped poster's video was uploaded")
	}
	host.st[testGuild] = skipping(ModeAuto, friendPoster)
	a.onMessage(context.Background(), msgWithID(testGuild, "300000000000000062"))
	waitIdle(t, a)
	if n := host.uploadCount(); n != 1 {
		t.Fatalf("after the refresh dropped him: %d box calls, want 1", n)
	}
}

// Pin: a poster put on the list while his reply waits uploads nothing — not
// on a press (the reply goes, as for a server switched off), and not when the
// reply's timer finds the server switched to Auto.
func TestSkipAddedWhileAReplyWaits(t *testing.T) {
	host := oneGuild(skipping(ModePrompt))
	a, rest, _ := guildAuto(t, host, nil, 0)
	a.onMessage(context.Background(), videoMessage(testGuild))
	host.st[testGuild] = skipping(ModePrompt, testPoster)
	ans, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	if ans == nil || ans["type"] != callbackDeferUpdate || start == nil {
		t.Fatalf("a press for a skipped poster: %v", ans)
	}
	start()
	waitReplyGone(t, rest)
	if host.uploadCount() != 0 {
		t.Fatal("a press for a skipped poster uploaded")
	}

	host2 := oneGuild(skipping(ModePrompt))
	b, rest2, timers := guildAuto(t, host2, nil, 0)
	b.onMessage(context.Background(), videoMessage(testGuild))
	host2.st[testGuild] = skipping(ModeAuto, testPoster)
	timers.all()[0].fire()
	waitReplyGone(t, rest2)
	waitIdle(t, b)
	if host2.uploadCount() != 0 {
		t.Fatal("a reply's timer uploaded a skipped poster's video under Auto")
	}
}

func waitReplyGone(t *testing.T, rest *fakeREST) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, c := range rest.snapshot() {
			if c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/"+testReply) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reply stayed: %+v", rest.snapshot())
		}
		time.Sleep(5 * time.Millisecond) // sleep: polling the delete the press or the timer starts
	}
}
