package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"clippet/internal/autobot"
)

// Lane `autoskip` (§20 erratum 334, amended 2026-10-19): the plane's `skip`
// list — the members who route their uploads to their own box — reaches the
// modes as Settings.Skip, replaced on every refresh; a record without it is
// today's.

// Pin: `skip` is the wire name, 200 ids parse into the set, a record without
// it skips nobody, and a refresh after the minute REPLACES the set — an id the
// plane dropped is no longer skipped, one it kept still is.
func TestSkipListFromThePlane(t *testing.T) {
	var wire guildRecord
	if err := json.Unmarshal([]byte(`{"on":true,"mode":"auto","u":"x","host":"h","skip":["1419000000000000111"]}`), &wire); err != nil ||
		len(wire.Skip) != 1 || wire.Skip[0] != "1419000000000000111" {
		t.Fatalf("the wire's skip: %+v %v", wire, err)
	}

	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("14190000000000%05d", i)
	}
	r := liveRecord(t, autobot.ModeAuto)
	r.Skip = ids
	plane := &fakePlane{records: map[string]guildRecord{tGuild: r}}
	h, c, _ := testHost(t, plane, &fakeBox{})
	ctx := context.Background()
	s, err := h.Settings(ctx, tGuild)
	if err != nil || len(s.Skip) != 200 || !s.Skip[ids[0]] || !s.Skip[ids[199]] || s.Skip["1419000000000099999"] {
		t.Fatalf("200 ids: %d in the set, err %v", len(s.Skip), err)
	}

	plane.mu.Lock()
	r.Skip = ids[1:2]
	plane.records[tGuild] = r
	plane.mu.Unlock()
	c.add(61 * time.Second)
	s, _ = h.Settings(ctx, tGuild)
	if len(s.Skip) != 1 || s.Skip[ids[0]] || !s.Skip[ids[1]] {
		t.Fatalf("after the refresh: %v, want only %s (replaced, never merged)", s.Skip, ids[1])
	}

	plane.mu.Lock()
	r.Skip = nil
	plane.records[tGuild] = r
	plane.mu.Unlock()
	c.add(61 * time.Second)
	if s, _ = h.Settings(ctx, tGuild); s.Skip != nil || s.Mode != autobot.ModeAuto {
		t.Fatalf("a record without skip: %+v, want nobody skipped and the mode as today", s)
	}
}

// Pin: a skipped poster's decision is the decision line at DEBUG with one
// field more, skip=person — absent at the process's INFO level — and it counts
// no video.
func TestSkipDecisionIsDebug(t *testing.T) {
	d := autobot.Decision{Event: "video", Guild: tGuild, Channel: "1419000000000000003", Message: "1419000000000000005",
		Mode: "auto", Outcome: "ignored", Skip: "person"}

	var info bytes.Buffer
	m := newMetrics(newLogger(&info, "json"))
	m.Decide(d)
	if info.Len() != 0 {
		t.Fatalf("a skip logged at INFO: %s", info.String())
	}
	m.mu.Lock()
	videos := len(m.videos)
	m.mu.Unlock()
	if videos != 0 {
		t.Fatalf("a skip counted a video: %v", m.videos)
	}

	var dbg bytes.Buffer
	m2 := newMetrics(slog.New(slog.NewJSONHandler(&dbg, &slog.HandlerOptions{Level: slog.LevelDebug})))
	m2.Decide(d)
	lines := strings.Split(strings.TrimSpace(dbg.String()), "\n")
	var got map[string]any
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &got) != nil {
		t.Fatalf("lines: %q", lines)
	}
	if got["level"] != "DEBUG" || got["msg"] != "decision" || got["skip"] != "person" || got["outcome"] != "ignored" {
		t.Fatalf("the skip line: %s", lines[0])
	}
}
