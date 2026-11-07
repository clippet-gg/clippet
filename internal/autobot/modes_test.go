package autobot

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Pin: Prompt's reply carries ONE button, `Upload to Clippet`, the press id,
// no text, as a reply to the video that pings nobody.
func TestPromptOfferIsOneButton(t *testing.T) {
	a, rest, host, timers := newModeAuto(t, ModePrompt, true)
	a.onMessage(context.Background(), videoMessage(testGuild))
	calls := rest.waitCalls(t, 1)
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/channels/"+testChannel+"/messages" {
		t.Fatalf("calls = %+v", calls)
	}
	b := calls[0].Body
	if _, has := b["content"]; has {
		t.Fatalf("the offer carries text: %v", b["content"])
	}
	rows, _ := b["components"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", b["components"])
	}
	buttons, _ := rows[0].(map[string]any)["components"].([]any)
	if len(buttons) != 1 {
		t.Fatalf("the offer has %d buttons, want ONE: %v", len(buttons), buttons)
	}
	btn := buttons[0].(map[string]any)
	if btn["label"] != LabelUpload || btn["custom_id"] != PressID {
		t.Fatalf("button = %v", btn)
	}
	ref := b["message_reference"].(map[string]any)
	if ref["message_id"] != testMessage {
		t.Fatalf("not a reply to the video: %v", ref)
	}
	if am := b["allowed_mentions"].(map[string]any); am["replied_user"] != false {
		t.Fatalf("the reply pings: %v", am)
	}
	if host.uploadCount() != 0 {
		t.Fatal("Prompt uploaded without a press")
	}
	if ts := timers.all(); len(ts) != 1 || ts[0].d != 10*time.Second {
		t.Fatalf("the reply's timer = %+v, want one of 10 s", ts)
	}
}

// Pin: the timer deletes the reply at N s — and not after a press.
func TestPromptTimerDeletesReplyUnlessPressed(t *testing.T) {
	a, rest, _, timers := newModeAuto(t, ModePrompt, true)
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	timers.all()[0].fire()
	calls := rest.waitCalls(t, 2)
	if c := calls[1]; c.Method != http.MethodDelete || c.Path != "/channels/"+testChannel+"/messages/"+testReply {
		t.Fatalf("the timer did %+v, want the reply deleted", c)
	}

	// A pressed reply: the timer is stopped, and firing it changes nothing.
	b, rest2, _, timers2 := newModeAuto(t, ModePrompt, true)
	b.onMessage(context.Background(), videoMessage(testGuild))
	rest2.waitCalls(t, 1)
	answer, start := b.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	if start == nil {
		t.Fatalf("the poster's press was refused: %v", answer)
	}
	tm := timers2.all()[0]
	if !tm.stopped.Load() {
		t.Fatal("a press did not stop the reply's timer")
	}
	tm.f() // the timer's func, as if it had raced the press
	if n := len(rest2.snapshot()); n != 1 {
		t.Fatalf("a pressed reply's timer made a call: %+v", rest2.snapshot())
	}
}

// Pin: a press answers `Uploading…` to the presser alone, and then — in this
// order — the upload, the link posted, the original deleted, the reply deleted.
func TestPressOrder(t *testing.T) {
	a, rest, host, _ := newModeAuto(t, ModePrompt, true)
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	answer, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	data, _ := answer["data"].(map[string]any)
	if answer["type"] != callbackMessage || data["content"] != LineUploading || data["flags"] != flagEphemeral {
		t.Fatalf("answer = %v", answer)
	}
	if host.uploadCount() != 0 {
		t.Fatal("the upload started before the answer was delivered")
	}
	start()
	rest.waitCalls(t, 4)
	waitIdle(t, a)
	calls := rest.snapshot()
	if host.uploadCount() != 1 {
		t.Fatalf("uploads = %d", host.uploadCount())
	}
	want := []struct{ method, path string }{
		{http.MethodPost, "/channels/" + testChannel + "/messages"},                  // the offer
		{http.MethodPost, "/channels/" + testChannel + "/messages"},                  // the link
		{http.MethodDelete, "/channels/" + testChannel + "/messages/" + testMessage}, // the original
		{http.MethodDelete, "/channels/" + testChannel + "/messages/" + testReply},   // the reply
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i, w := range want {
		if calls[i].Method != w.method || calls[i].Path != w.path {
			t.Fatalf("call %d = %s %s, want %s %s (all: %+v)", i, calls[i].Method, calls[i].Path, w.method, w.path, calls)
		}
	}
	if !strings.HasPrefix(calls[1].Body["content"].(string), testLink) {
		t.Fatalf("the link message = %v", calls[1].Body)
	}
	if _, has := calls[1].Body["message_reference"]; has {
		t.Fatal("the link is a reply to the original, which is about to go")
	}
}

// Pin: someone who is neither the poster nor a manager gets one private line;
// Manage Server may press anybody's.
func TestPressWhoMay(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModePrompt, true)
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	answer, start := a.Press(context.Background(), pressBody(testGuild, "900000000000000009", "0"))
	if start != nil || answer["data"].(map[string]any)["content"] != LineNotYours {
		t.Fatalf("a stranger's press = %v", answer)
	}
	_, start = a.Press(context.Background(), pressBody(testGuild, "900000000000000009", "32")) // Manage Server
	if start == nil {
		t.Fatal("a manager's press was refused")
	}
}

// Pin: Auto posts no reply and no button: the link, then the original deleted.
func TestAutoNoReply(t *testing.T) {
	a, rest, host, timers := newModeAuto(t, ModeAuto, true)
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 2)
	waitIdle(t, a)
	calls := rest.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Method != http.MethodPost || calls[0].Body["components"] != nil || !strings.HasPrefix(calls[0].Body["content"].(string), testLink) {
		t.Fatalf("Auto's first call is not the link: %+v", calls[0])
	}
	if calls[1].Method != http.MethodDelete || calls[1].Path != "/channels/"+testChannel+"/messages/"+testMessage {
		t.Fatalf("Auto's second call is not the original's delete: %+v", calls[1])
	}
	if host.uploadCount() != 1 || len(timers.all()) != 0 {
		t.Fatalf("uploads=%d timers=%d", host.uploadCount(), len(timers.all()))
	}
}

// Pin: the original is never deleted when the link did not post.
func TestOriginalKeptWhenLinkFails(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModeAuto, true)
	rest.respond = func(method, path string) Response {
		if method == http.MethodPost {
			return Response{Status: 500}
		}
		return Response{}
	}
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	waitIdle(t, a)
	for _, c := range rest.snapshot() {
		if c.Method == http.MethodDelete {
			t.Fatalf("the original was deleted although the link failed: %+v", rest.snapshot())
		}
	}
}

// Pin: the notice line under the link, on and off.
func TestNoticeOnOff(t *testing.T) {
	for _, notice := range []bool{true, false} {
		a, rest, _, _ := newModeAuto(t, ModeAuto, notice)
		a.onMessage(context.Background(), videoMessage(testGuild))
		rest.waitCalls(t, 2)
		waitIdle(t, a)
		got := rest.snapshot()[0].Body["content"]
		want := testLink
		if notice {
			want = testLink + "\n-# " + LineNotice
		}
		if got != want {
			t.Fatalf("notice=%v: content = %q, want %q", notice, got, want)
		}
	}
	if LineNotice != "Videos posted here become Clippet links." {
		t.Fatalf("the notice's words moved: %q", LineNotice)
	}
}

func forbidDeleteOf(id string) func(method, path string) Response {
	return func(method, path string) Response {
		if method == http.MethodDelete && strings.HasSuffix(path, "/"+id) {
			return Response{Status: http.StatusForbidden, Body: []byte(`{"code":50013}`)}
		}
		return Response{}
	}
}

// Pin: Prompt without Manage Messages — the link stays, the presser alone is
// told, and the delete is tried once.
func TestPromptMissingManageMessages(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModePrompt, true)
	rest.respond = forbidDeleteOf(testMessage)
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	_, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	start()
	rest.waitCalls(t, 5)
	waitIdle(t, a)
	deletes, said := 0, false
	for _, c := range rest.snapshot() {
		if c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/"+testMessage) {
			deletes++
		}
		if strings.HasPrefix(c.Path, "/webhooks/"+testApp+"/itok") && c.Body["content"] == LineNeedsManage && c.Body["flags"] == float64(flagEphemeral) {
			said = true
		}
		if strings.HasSuffix(c.Path, "/messages") && c.Body["content"] == LineNeedsManage {
			t.Fatal("Prompt told the channel, not the presser")
		}
	}
	if deletes != 1 || !said {
		t.Fatalf("deletes=%d said=%v: %+v", deletes, said, rest.snapshot())
	}
}

// Pin: Auto without Manage Messages — ONE channel line per day, however many
// videos.
func TestAutoMissingManageMessagesOncePerDay(t *testing.T) {
	a, rest, _, _ := newModeAuto(t, ModeAuto, true)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	rest.respond = forbidDeleteOf(testMessage)
	lines := func() int {
		n := 0
		for _, c := range rest.snapshot() {
			if c.Method == http.MethodPost && c.Body["content"] == LineNeedsManage {
				n++
			}
		}
		return n
	}
	for i := 0; i < 3; i++ {
		a.onMessage(context.Background(), videoMessage(testGuild))
		waitIdle(t, a)
	}
	if lines() != 1 {
		t.Fatalf("Auto said the line %d times in one day: %+v", lines(), rest.snapshot())
	}
	now = now.Add(25 * time.Hour)
	a.onMessage(context.Background(), videoMessage(testGuild))
	waitIdle(t, a)
	if lines() != 2 {
		t.Fatalf("a day later the line was said %d times in all", lines())
	}
}

// Pin: a server that is not bound is ignored — no reply, no upload, no call;
// its press is ignored (on the socket not even answered) and nothing starts.
func TestUnboundGuildIgnored(t *testing.T) {
	for _, mode := range []string{ModePrompt, ModeAuto} {
		a, rest, host, _ := newModeAuto(t, mode, true)
		a.onMessage(context.Background(), videoMessage(otherGuild))
		waitIdle(t, a)
		if n := len(rest.snapshot()); n != 0 || host.uploadCount() != 0 {
			t.Fatalf("%s: an unbound server's video made %d calls and %d uploads", mode, n, host.uploadCount())
		}
		answer, start := a.Press(context.Background(), pressBody(otherGuild, testPoster, "8"))
		if start != nil || answer != nil {
			t.Fatalf("%s: an unbound server's press = %v", mode, answer)
		}
		a.onSocketInteraction(context.Background(), pressBody(otherGuild, testPoster, "8"))
		waitIdle(t, a)
		if n := len(rest.snapshot()); n != 0 {
			t.Fatalf("%s: an unbound server's press was answered on the socket: %+v", mode, rest.snapshot())
		}
	}
}

// Pin: a press while the server's queue is full is told `Busy.` on the press
// itself, and its reply keeps waiting.
func TestPressBusy(t *testing.T) {
	a, rest, host, timers := newModeAuto(t, ModePrompt, true)
	host.gate = make(chan struct{})
	defer close(host.gate)
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	a.enqueue(job{guild: testGuild, channel: testChannel, original: testMessage}) // running, held at the gate
	deadline := time.Now().Add(5 * time.Second)
	for host.uploadCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond) // sleep: waiting for the drain goroutine to take the first job
	}
	for i := 0; i < defaultQueueCap; i++ { // ten waiting
		if !a.enqueue(job{guild: testGuild, channel: testChannel, original: testMessage}) {
			t.Fatalf("enqueue %d refused", i)
		}
	}
	if !a.queueFull(testGuild) {
		t.Fatal("the queue is not full")
	}
	answer, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	if start != nil || answer["data"].(map[string]any)["content"] != LineBusy {
		t.Fatalf("a press on a full queue = %v", answer)
	}
	if timers.all()[0].stopped.Load() {
		t.Fatal("a busy press stopped the reply's timer")
	}
}

// A message that is not applicable, or a bot's, is ignored silently.
func TestNotApplicableIgnored(t *testing.T) {
	a, rest, host, _ := newModeAuto(t, ModeAuto, true)
	img := `{"id":"` + testMessage + `","channel_id":"` + testChannel + `","guild_id":"` + testGuild +
		`","author":{"id":"` + testPoster + `"},"attachments":[{"id":"1","filename":"a.png","url":"x","size":1,"content_type":"image/png"}]}`
	a.onMessage(context.Background(), []byte(img))
	bot := strings.Replace(string(videoMessage(testGuild)), `"author":{"id":"`+testPoster+`"}`, `"author":{"id":"`+testPoster+`","bot":true}`, 1)
	a.onMessage(context.Background(), []byte(bot))
	waitIdle(t, a)
	if len(rest.snapshot()) != 0 || host.uploadCount() != 0 {
		t.Fatalf("calls=%+v uploads=%d", rest.snapshot(), host.uploadCount())
	}
}

// Pin: one upload at a time per server, at most QueueCap waiting; the next
// video is dropped with a log line, never a crash.
func TestQueueCap(t *testing.T) {
	a, _, host, _ := newModeAuto(t, ModeAuto, true)
	host.gate = make(chan struct{})
	a.onMessage(context.Background(), videoMessage(testGuild)) // running, held at the gate
	deadline := time.Now().Add(5 * time.Second)
	for host.uploadCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond) // sleep: waiting for the first upload to start
	}
	if host.uploadCount() != 1 {
		t.Fatal("the first upload never started")
	}
	accepted := 0
	for i := 0; i < defaultQueueCap+3; i++ {
		if a.enqueue(job{guild: testGuild, channel: testChannel, original: testMessage}) {
			accepted++
		}
	}
	if accepted != defaultQueueCap {
		t.Fatalf("accepted %d behind a running upload, want %d", accepted, defaultQueueCap)
	}
	if host.uploadCount() != 1 {
		t.Fatalf("%d uploads ran at once in one server", host.uploadCount())
	}
	close(host.gate)
	waitIdle(t, a)
	if host.uploadCount() != 1+defaultQueueCap {
		t.Fatalf("uploads = %d", host.uploadCount())
	}
}

// Pin: an upload refusal reaches the presser as the host's own line.
func TestPressFailureLine(t *testing.T) {
	a, rest, host, _ := newModeAuto(t, ModePrompt, true)
	host.err = lineErr("That file is larger than this Clippet accepts.")
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	_, start := a.Press(context.Background(), pressBody(testGuild, testPoster, "0"))
	start()
	rest.waitCalls(t, 3)
	waitIdle(t, a)
	found := false
	for _, c := range rest.snapshot() {
		if c.Method == http.MethodDelete && strings.HasSuffix(c.Path, "/"+testMessage) {
			t.Fatal("the original was deleted after a failed upload")
		}
		if strings.HasPrefix(c.Path, "/webhooks/") && c.Body["content"] == string(host.err.(lineErr)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the presser was not told: %+v", rest.snapshot())
	}
}

type lineErr string

func (e lineErr) Error() string { return string(e) }
func (e lineErr) Line() string  { return string(e) }

// Pin: Press is ONE function for both doors — the socket's press answers
// through the callback route and then starts; a press whose answer failed
// uploads nothing.
func TestSocketPressDoor(t *testing.T) {
	a, rest, host, _ := newModeAuto(t, ModePrompt, false)
	a.onMessage(context.Background(), videoMessage(testGuild))
	rest.waitCalls(t, 1)
	rest.respond = func(method, path string) Response {
		if strings.HasSuffix(path, "/callback") {
			return Response{Status: 500}
		}
		return Response{}
	}
	a.onSocketInteraction(context.Background(), pressBody(testGuild, testPoster, "0"))
	waitIdle(t, a)
	if host.uploadCount() != 0 {
		t.Fatal("a press whose answer did not reach Discord uploaded")
	}

	b, rest2, host2, _ := newModeAuto(t, ModePrompt, false)
	b.onMessage(context.Background(), videoMessage(testGuild))
	rest2.waitCalls(t, 1)
	b.onSocketInteraction(context.Background(), pressBody(testGuild, testPoster, "0"))
	rest2.waitCalls(t, 5)
	waitIdle(t, b)
	cb := rest2.snapshot()[1]
	if !strings.HasSuffix(cb.Path, "/callback") || cb.Auth || cb.Body["data"].(map[string]any)["content"] != LineUploading {
		t.Fatalf("the socket's answer = %+v", cb)
	}
	if host2.uploadCount() != 1 {
		t.Fatal("the socket's press did not upload")
	}
}

func TestHasManage(t *testing.T) {
	for perms, want := range map[string]bool{"0": false, "8": true, "32": true, "40": true, "16": false, "": false, "x": false,
		"1099511627776": false, "1099511627808": true} {
		if hasManage(perms) != want {
			t.Errorf("hasManage(%q) = %v", perms, !want)
		}
	}
}

// The brief's strings, as written.
func TestWords(t *testing.T) {
	for got, want := range map[string]string{
		LabelUpload: "Upload to Clippet", LineUploading: "Uploading…",
		LineNeedsManage: "Clippet Auto needs Manage Messages to remove the original.",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
