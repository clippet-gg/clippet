package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"clippet/internal/urls"
)

// The late card (§20 erratum 330): a link posted before its card was ready is
// redrawn once the worker says the card is (Bot.CardReady), counted `?v=2`,
// `?v=3`, … with the form's late title; never a link that was ready at post,
// never a duplicate's, never after a restart.

func lateIngest(context.Context, IngestSource) (IngestResult, error) {
	return IngestResult{ClipID: testClipID, CardReady: false}, nil
}

func isPatch(c apiCall) bool { return c.method == http.MethodPatch }

// linkBody is the PATCH/POST body of the clip's link with an optional query.
func linkBody(query string) string {
	return `{"content":"https://clips.example/` + testClipID + query + `","allowed_mentions":{"parse":[]}}`
}

// shutdown drains the bot so a count read afterwards is final.
func (f *botFixture) shutdown(t *testing.T) []apiCall {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = f.bot.Shutdown(ctx)
	return f.callsSnapshot()
}

// TestBotLateCardRedrawsTheLinkOnce: a held link — the deferred reply
// (`/upload` without the form, an old `/clip file:`, Enrich's upload) and the
// form's follow-up — whose card becomes ready is PATCHed exactly once, to the
// link plus `?v=2`, on the message it was posted as; a second word that the
// card is ready edits nothing. The same on a managed instance.
func TestBotLateCardRedrawsTheLinkOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		job     func(*botFixture) ClipJob
		managed bool
		message string // the message id the redraw names
	}{
		{"deferred", func(f *botFixture) ClipJob { return okJob(f, lateIngest) }, false, "@original"},
		{"deferred managed", func(f *botFixture) ClipJob { return okJob(f, lateIngest) }, true, "@original"},
		{"form", func(f *botFixture) ClipJob { return formJob(f, lateIngest) }, false, linkMsgID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBotFixture(t, http.StatusOK, "video bytes")
			if tc.managed {
				managedBot(t, f)
			}
			recordAPI(f, discordLike)
			f.bot.Clip(tc.job(f))
			f.waitCalls(t, "the link held", func([]apiCall) bool { return linkHeld(f.bot) })
			f.bot.CardReady(testClipID)
			calls := f.waitCalls(t, "outcome=edited", func([]apiCall) bool {
				return strings.Contains(f.logs.String(), "outcome=edited")
			})
			redraws := []apiCall{}
			for _, c := range calls {
				if isPatch(c) && string(c.body) == linkBody("?v=2") {
					redraws = append(redraws, c)
				}
			}
			if len(redraws) != 1 || redraws[0].path != f.hook()+"/messages/"+tc.message || redraws[0].auth != "" {
				t.Fatalf("want one redraw of %s to ?v=2: %v", tc.message, describe(calls))
			}
			if !strings.Contains(f.logs.String(), "reason=card outcome=edited") {
				t.Fatalf("the redraw line does not name its reason:\n%s", f.logs.String())
			}
			f.bot.CardReady(testClipID)
			calls = f.shutdown(t)
			if n := count(calls, func(c apiCall) bool { return isPatch(c) && strings.Contains(string(c.body), "?v=") }); n != 1 {
				t.Fatalf("%d redraws, want one: %v", n, describe(calls))
			}
			if want := map[bool]int{false: 0, true: 1}[tc.message == linkMsgID]; f.bot.links.len() != want {
				t.Fatalf("%d links held, want %d (a form's link is still owed its title)", f.bot.links.len(), want)
			}
			if strings.Contains(f.logs.String(), testIntToken) {
				t.Fatal("the interaction token reached the log")
			}
		})
	}
}

// TestBotCardReadyAtPostIsNeverRedrawn: a link posted with its card ready is
// not held for the card (the form's still is, for its title only), so the
// worker's word redraws nothing — and logs nothing at INFO.
func TestBotCardReadyAtPostIsNeverRedrawn(t *testing.T) {
	for _, form := range []bool{false, true} {
		f := newBotFixture(t, http.StatusOK, "video bytes")
		recordAPI(f, discordLike)
		job := okJob(f, okIngest)
		if form {
			job = formJob(f, okIngest)
		}
		f.bot.Clip(job)
		f.waitCalls(t, "the link", func(c []apiCall) bool { return len(c) > 0 })
		if !form && f.bot.links.len() != 0 {
			t.Fatal("a deferred link whose card was ready was held")
		}
		f.bot.CardReady(testClipID)
		calls := f.shutdown(t)
		if n := count(calls, func(c apiCall) bool { return strings.Contains(string(c.body), "?v=") }); n != 0 {
			t.Fatalf("form=%v: a ready-at-post link was redrawn: %v", form, describe(calls))
		}
	}
}

// TestBotLateCardThenLateTitleStepsTheVersion: Discord ignores an edit whose
// content is unchanged, so the second redraw of a link — the late card, then
// the late title, or the reverse — is `?v=3`, each with its reason.
func TestBotLateCardThenLateTitleStepsTheVersion(t *testing.T) {
	for _, order := range [][2]string{{"card", "title"}, {"title", "card"}} {
		t.Run(order[0]+"_then_"+order[1], func(t *testing.T) {
			f := newBotFixture(t, http.StatusOK, "video bytes")
			recordAPI(f, discordLike)
			f.bot.Clip(formJob(f, lateIngest))
			f.waitCalls(t, "the link held", func([]apiCall) bool { return linkHeld(f.bot) })
			ask := func(what string) {
				if what == "card" {
					f.bot.CardReady(testClipID)
				} else {
					f.bot.Redraw(testClipID)
				}
			}
			ask(order[0])
			f.waitCalls(t, "?v=2", func(c []apiCall) bool {
				return count(c, func(c apiCall) bool { return string(c.body) == linkBody("?v=2") }) == 1 &&
					strings.Contains(f.logs.String(), "outcome=edited")
			})
			ask(order[1])
			calls := f.waitCalls(t, "?v=3", func(c []apiCall) bool {
				return count(c, func(c apiCall) bool { return string(c.body) == linkBody("?v=3") }) == 1
			})
			calls = append(calls[:0:0], f.shutdown(t)...)
			if n := count(calls, isPatch); n != 2 {
				t.Fatalf("%d edits, want two: %v", n, describe(calls))
			}
			for _, c := range calls {
				if isPatch(c) && c.path != f.hook()+"/messages/"+linkMsgID {
					t.Fatalf("a redraw went to %s", c.path)
				}
			}
			logs := f.logs.String()
			if !strings.Contains(logs, "reason=card outcome=edited") || !strings.Contains(logs, "reason=title outcome=edited") {
				t.Fatalf("the two redraws do not name their reasons:\n%s", logs)
			}
			if f.bot.links.len() != 0 {
				t.Fatal("a link owed nothing more is still held")
			}
		})
	}
}

// TestBotLateCardOfADuplicateIsNeverHeld: the duplicate's answer (§20 erratum
// 326) is the existing clip's link, never held — not for the form, not for a
// card.
func TestBotLateCardOfADuplicateIsNeverHeld(t *testing.T) {
	for _, form := range []bool{false, true} {
		f := newBotFixture(t, http.StatusOK, "video bytes")
		recordAPI(f, discordLike)
		dup := func(context.Context, IngestSource) (IngestResult, error) {
			return IngestResult{ClipID: testClipID, CardReady: false, Existing: true}, nil
		}
		job := okJob(f, dup)
		if form {
			job = formJob(f, dup)
		}
		f.bot.Clip(job)
		f.waitCalls(t, "the link", func(c []apiCall) bool { return len(c) > 0 })
		f.bot.CardReady(testClipID)
		calls := f.shutdown(t)
		if f.bot.links.len() != 0 || count(calls, func(c apiCall) bool { return strings.Contains(string(c.body), "?v=") }) != 0 {
			t.Fatalf("form=%v: a duplicate was held or redrawn: %v", form, describe(calls))
		}
	}
}

// TestBotLateCardWhileThePostIsInFlight: the card becomes ready while the
// link's POST is in flight — the job redraws once Discord answers.
func TestBotLateCardWhileThePostIsInFlight(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	// A failure before the release must not leave the fake API blocked (the
	// server's Close would then wait for ever).
	defer releaseOnce.Do(func() { close(release) })
	recordAPI(f, func(m, p string, body []byte) (int, string) {
		if m == http.MethodPost && !strings.Contains(string(body), `"flags":64`) {
			close(arrived)
			<-release
		}
		return discordLike(m, p, body)
	})
	f.bot.Clip(formJob(f, lateIngest))
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the link was never posted")
	}
	f.bot.CardReady(testClipID)
	if !strings.Contains(f.logs.String(), "reason=card outcome=after_post") {
		t.Fatalf("not deferred to the post:\n%s", f.logs.String())
	}
	releaseOnce.Do(func() { close(release) })
	calls := f.waitCalls(t, "the redraw", func(c []apiCall) bool { return count(c, isPatch) > 0 })
	if p := calls[indexOf(calls, isPatch)]; p.path != f.hook()+"/messages/"+linkMsgID || string(p.body) != linkBody("?v=2") {
		t.Fatalf("the redraw: %s %s", p.path, p.body)
	}
}

// TestBotHoldReplyRedrawsTheTypeFourAnswer: `/clip link:` (and Enrich on a
// link) answers on the interaction's own response; when that clip's card is
// not ready the endpoint holds the reply, and the worker's word PATCHes
// `@original` to `?v=2`, once.
func TestBotHoldReplyRedrawsTheTypeFourAnswer(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "")
	recordAPI(f, discordLike)
	f.bot.HoldReply(testIntToken, testClipID)
	f.bot.CardReady(testClipID)
	f.bot.CardReady(testClipID)
	calls := f.waitCalls(t, "the redraw", func(c []apiCall) bool { return count(c, isPatch) > 0 })
	calls = append(calls[:0:0], f.shutdown(t)...)
	if len(calls) != 1 || calls[0].path != f.hook()+"/messages/@original" || string(calls[0].body) != linkBody("?v=2") {
		t.Fatalf("want one PATCH of @original to ?v=2: %v", describe(calls))
	}
}

// TestBotRestartForgetsTheLateCard: the links live in memory. A bot that was
// shut down says so and edits nothing; a NEW bot — the restarted process —
// holds nothing, so the worker's word reaches no message (the link stays bare;
// a later paste of it draws the card).
func TestBotRestartForgetsTheLateCard(t *testing.T) {
	f := newBotFixture(t, http.StatusOK, "video bytes")
	recordAPI(f, discordLike)
	f.bot.Clip(okJob(f, lateIngest))
	f.waitCalls(t, "the link held", func([]apiCall) bool { return linkHeld(f.bot) })
	f.shutdown(t)
	f.bot.CardReady(testClipID)
	if !strings.Contains(f.logs.String(), "reason=card outcome=restart") {
		t.Fatalf("a shut-down bot did not say restart:\n%s", f.logs.String())
	}
	base, _ := url.Parse("https://clips.example")
	again := NewBot(BotDeps{AppID: testAppID, Token: testBotToken, URLs: urls.New(base, "test"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIHost: strings.TrimPrefix(f.api.URL, "http://"), APIScheme: "http"})
	again.CardReady(testClipID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = again.Shutdown(ctx)
	if n := count(f.callsSnapshot(), isPatch); n != 1 {
		t.Fatalf("%d PATCHes, want only the first post's deferred edit", n)
	}
}

// TestLinkTableHoldsEveryLinkOfAClip: the upload's own link and a `/clip link:`
// of the same clip are two held links; the card's word redraws both, each at a
// version no other edit of that clip used.
func TestLinkTableHoldsEveryLinkOfAClip(t *testing.T) {
	tb := newLinkTable()
	t0 := time.Unix(1_800_000_000, 0)
	a := tb.begin(testClipID, "tokA", "", reasonCard|reasonTitle, t0.Add(linkLife), t0)
	tb.posted(a, "902")
	tb.begin(testClipID, "tokB", originalMessage, reasonCard, t0.Add(linkLife), t0)
	tasks, later := tb.take(testClipID, reasonCard, t0)
	if later || len(tasks) != 2 || tasks[0].version == tasks[1].version {
		t.Fatalf("tasks %+v later=%v", tasks, later)
	}
	if tb.len() != 1 {
		t.Fatalf("%d held; want the form's link alone (its title is still owed)", tb.len())
	}
	tasks, _ = tb.take(testClipID, reasonTitle, t0)
	if len(tasks) != 1 || tasks[0].version != 4 || tasks[0].messageID != "902" || tb.len() != 0 {
		t.Fatalf("the title's redraw: %+v (held %d)", tasks, tb.len())
	}
}
