package autobot

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Lane `autoperson` (§20 erratum 333, amended 2026-10-19; erratum 342): on a
// PERSON-scoped binding Clippet Auto on the box acts only on the linked
// member's videos — a friend's box never takes another member's. Each pin
// fails with the personAllows checks reverted.

const otherPoster = "400000000000000002"

// scopedHost is the box's fakeHost with a binding scope.
type scopedHost struct {
	*fakeHost
	mu     sync.Mutex
	person bool
	id     string
	err    error
}

func (s *scopedHost) Person(context.Context, string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.person, s.id, s.err
}

func (s *scopedHost) set(person bool, id string) {
	s.mu.Lock()
	s.person, s.id = person, id
	s.mu.Unlock()
}

func scopedAuto(t *testing.T, mode string, person bool, id string) (*Auto, *fakeREST, *scopedHost, *fakeTimers, *recObs) {
	t.Helper()
	a, rest, host, timers := newModeAuto(t, mode, true)
	sh := &scopedHost{fakeHost: host, person: person, id: id}
	a.SetHost(sh)
	obs := &recObs{}
	a.obs = obs
	return a, rest, sh, timers, obs
}

// Pin: on a person binding, another poster's video — under Prompt and under
// Auto — makes no box call, no REST call (no reply), arms no timer, and leaves
// one decision marked skip=person that names no poster. A person binding with
// no linked id, or a scope that cannot be read, acts for nobody.
func TestPersonBindingIgnoresOtherPosters(t *testing.T) {
	for _, mode := range []string{ModePrompt, ModeAuto} {
		for _, c := range []struct {
			name string
			id   string
			err  error
		}{{"another poster", otherPoster, nil}, {"no linked id", "", nil}, {"unread scope", testPoster, errors.New("db")}} {
			a, rest, host, timers, obs := scopedAuto(t, mode, true, c.id)
			host.err = c.err
			a.onMessage(context.Background(), videoMessage(testGuild))
			waitIdle(t, a)
			if n := host.uploadCount(); n != 0 {
				t.Fatalf("%s/%s: %d box calls", mode, c.name, n)
			}
			if calls := rest.snapshot(); len(calls) != 0 {
				t.Fatalf("%s/%s: reached Discord's REST: %+v", mode, c.name, calls)
			}
			if ts := timers.all(); len(ts) != 0 {
				t.Fatalf("%s/%s: armed %d reply timers", mode, c.name, len(ts))
			}
			obs.mu.Lock()
			ds := append([]Decision(nil), obs.decisions...)
			obs.mu.Unlock()
			if len(ds) != 1 || ds[0].Skip != "person" || ds[0].Outcome != "ignored" || ds[0].Mode != mode {
				t.Fatalf("%s/%s: decisions %+v, want one ignored with skip=person", mode, c.name, ds)
			}
			for _, f := range []string{ds[0].Event, ds[0].Guild, ds[0].Channel, ds[0].Message, ds[0].Mode, ds[0].Outcome, ds[0].Skip} {
				if strings.Contains(f, testPoster) || strings.Contains(f, otherPoster) {
					t.Fatalf("%s/%s: the decision carries a Discord user id: %+v", mode, c.name, ds[0])
				}
			}
		}
	}
}

// Pin: the linked poster on a person binding, and every poster on a server
// binding, is served as today — Prompt's reply and timer, Auto's one upload —
// with no skip on any decision.
func TestPersonBindingServesTheLinkedPoster(t *testing.T) {
	for _, c := range []struct {
		name   string
		person bool
		id     string
	}{{"person, linked poster", true, testPoster}, {"server", false, ""}, {"server, other id", false, otherPoster}} {
		a, rest, _, timers, obs := scopedAuto(t, ModePrompt, c.person, c.id)
		a.onMessage(context.Background(), videoMessage(testGuild))
		offered := false
		for _, k := range rest.snapshot() {
			if k.Method == http.MethodPost && strings.HasSuffix(k.Path, "/messages") {
				offered = true
			}
		}
		if !offered || len(timers.all()) != 1 {
			t.Fatalf("%s: Prompt offered %v with %d timers", c.name, offered, len(timers.all()))
		}
		b, _, host, _, obs2 := scopedAuto(t, ModeAuto, c.person, c.id)
		b.onMessage(context.Background(), videoMessage(testGuild))
		waitIdle(t, b)
		if n := host.uploadCount(); n != 1 {
			t.Fatalf("%s: Auto made %d box calls, want 1", c.name, n)
		}
		for _, o := range []*recObs{obs, obs2} {
			o.mu.Lock()
			for _, d := range o.decisions {
				if d.Skip != "" {
					t.Fatalf("%s: a served video's decision carries skip=%q", c.name, d.Skip)
				}
			}
			o.mu.Unlock()
		}
	}
}

// Pin: a server re-bound to another person while the reply waits — a press
// (even a manager's) removes the reply and uploads nothing.
func TestPersonBindingAppliedAtThePress(t *testing.T) {
	a, rest, host, _, _ := scopedAuto(t, ModePrompt, false, "")
	a.onMessage(context.Background(), videoMessage(testGuild))
	host.set(true, otherPoster)
	ans, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "32"))
	if ans == nil || ans["type"] != callbackDeferUpdate || start == nil {
		t.Fatalf("a press after the re-bind: %v", ans)
	}
	start()
	waitReplyGone(t, rest)
	waitIdle(t, a)
	if host.uploadCount() != 0 {
		t.Fatal("a press after the re-bind uploaded another member's video")
	}
}
