package main

// backup_schedule.go — timed backups of the DATABASE, OFF by default (ROADMAP
// #40 item 4, DESIGN.md §20 erratum 321).
//
// Set BACKUP_EVERY and BACKUP_DIR and `clippet serve` runs what an operator's
// cron line ran: `admin backup <BACKUP_DIR> --rotate <BACKUP_KEEP>`. Not a
// second implementation of it — the loop calls the verb's own backupInto, so
// the snapshot's name, its 0600 mode, its `.part-` file and the rotation that
// only ever touches minted names are the verb's, unchanged.
//
// The rules, each of them small:
//
//   - One goroutine, one run at a time: the loop cannot overlap itself.
//   - The directory is read for SCHEDULING only at boot. The anchor is the
//     newest minted snapshot whose time is not after now; no anchor, or one
//     older than BACKUP_EVERY, runs at once, else the first run is one
//     interval after it — so a restart never resets the clock. After every
//     run, whatever its result, the next is that run's start + BACKUP_EVERY.
//     One rule, and a snapshot named in the future cannot stall it.
//   - A run is SKIPPED when BACKUP_DIR holds a `.part-` file younger than
//     stalePartAge: by the verb's own rule a run may still be writing it (a
//     manual `admin backup` into the same directory), or it is what a failed
//     run left behind. A skip is logged and moves the next run, and never
//     replaces the last real result — a skip after a failure must not make the
//     failure disappear from `admin doctor`.
//   - The loop never calls `backup(args)` and never creates BACKUP_DIR: the
//     verb's front door treats a missing path as a FILE destination, and an
//     unmounted volume must fail loudly rather than fill the container.
//   - Each run is bounded by the verb's own adminTimeout and recovers a panic:
//     db.Backup runs on the rw pool's single connection, so a stalled VACUUM
//     would hold every write of this process, and a backup must never be able
//     to take the server down.
//
// What the server did is written to DATA_DIR/backup-status.json, because
// `admin doctor` is another process — often a `--rm` container with no server
// at all — and cannot read this one's memory.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clippet/internal/config"
	"clippet/internal/db"
)

// backupStatusName is the status file's name under DATA_DIR. Not in BACKUP_DIR:
// it would show in rotation's `untouched=` count, and the backup directory may
// be the very thing that is broken.
const backupStatusName = "backup-status.json"

// Results a run records. A skip is not one of them (see the file comment).
const (
	backupResultOK     = "ok"
	backupResultFailed = "failed"
)

// backupStatus is the status file: the directory it describes, the last real
// run and the next one. Times are RFC 3339 UTC.
type backupStatus struct {
	Dir  string      `json:"dir"`
	Last *backupLast `json:"last,omitempty"`
	Next string      `json:"next"`
}

type backupLast struct {
	At     string `json:"at"`
	Result string `json:"result"`
	File   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
}

// readBackupStatus reads DATA_DIR's status file. A missing, unreadable or
// malformed file is reported as absent (ok=false), by the loop and by doctor
// alike: the file is a record, never an input anything depends on.
func readBackupStatus(dataDir string) (backupStatus, bool) {
	raw, err := os.ReadFile(filepath.Join(dataDir, backupStatusName))
	if err != nil {
		return backupStatus{}, false
	}
	var st backupStatus
	if err := json.Unmarshal(raw, &st); err != nil || st.Dir == "" {
		return backupStatus{}, false
	}
	return st, true
}

// writeBackupStatus replaces the status file atomically, 0600.
func writeBackupStatus(dataDir string, st backupStatus) error {
	return writeJSONAtomic(dataDir, backupStatusName, st)
}

// writeJSONAtomic replaces DATA_DIR/<name> with v as JSON, 0600, tmp + rename —
// the backup status file's writer, and the snapshot's (§20 erratum 343).
func writeJSONAtomic(dataDir, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dataDir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dataDir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// backupScheduler is the loop. now is injectable so the tests can drive the
// schedule's arithmetic without waiting for it.
type backupScheduler struct {
	cfg    config.Config
	db     *db.DB
	log    *slog.Logger
	now    func() time.Time
	status backupStatus
}

func newBackupScheduler(cfg config.Config, database *db.DB, log *slog.Logger) *backupScheduler {
	return &backupScheduler{cfg: cfg, db: database, log: log, now: time.Now}
}

// Run boots the schedule and runs until ctx is done. It is only started when
// cfg.BackupOn().
func (s *backupScheduler) Run(ctx context.Context) {
	next := s.boot()
	for {
		t := time.NewTimer(max(next.Sub(s.now()), 0))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		start := s.now()
		next = s.runOnce(ctx, start)
		if ctx.Err() != nil {
			return
		}
	}
}

// nextAfter is when the run after one that started at start is due. A skip
// (a fresh `.part-` in the directory) is retried once that part turns stale
// rather than a whole interval on: min(BACKUP_EVERY, an hour) — so a restart
// within an hour of an interrupted run, skipped at boot, is not a day without
// a snapshot (run 8; DEPLOY §15.6).
//
// A run that took longer than BACKUP_EVERY is followed at once (start+every is
// already past when it ends): only on a target slower than its own interval,
// which no sane setting is, and the next run then holds the rw connection
// straight after — accepted, not guarded (run 8).
func (s *backupScheduler) nextAfter(start time.Time, skipped bool) time.Time {
	if skipped {
		return start.Add(min(s.cfg.BackupEvery, stalePartAge))
	}
	return start.Add(s.cfg.BackupEvery)
}

// boot computes the first run from the directory (the only time the directory
// is read for scheduling), carries the last result over when the status file
// describes this BACKUP_DIR, writes the status and logs the schedule.
func (s *backupScheduler) boot() time.Time {
	now := s.now()
	next := firstBackupRun(s.cfg.BackupDir, s.cfg.BackupEvery, now)
	s.status = backupStatus{Dir: s.cfg.BackupDir}
	if prev, ok := readBackupStatus(s.cfg.DataDir); ok && prev.Dir == s.cfg.BackupDir {
		s.status.Last = prev.Last
	}
	s.status.Next = rfcTime(next)
	s.saveStatus()
	s.log.Info("backup scheduled", "every", s.cfg.BackupEvery, "dir", s.cfg.BackupDir,
		"keep", s.cfg.BackupKeep, "next", rfcTime(next))
	return next
}

// firstBackupRun is the boot rule. The anchor is the newest minted snapshot
// whose time is not after now (a name in the future — a clock that was once
// wrong, a copied-in file — is ignored rather than allowed to push the first
// run years out). No anchor, or anchor+every already past, means now.
func firstBackupRun(dir string, every time.Duration, now time.Time) time.Time {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return now
	}
	var anchor time.Time
	for _, e := range entries {
		if e.IsDir() || !snapshotRE.MatchString(e.Name()) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "clippet-"), ".db")
		at, err := time.Parse(snapshotStamp, stamp)
		if err != nil || at.After(now) {
			continue
		}
		if at.After(anchor) {
			anchor = at
		}
	}
	if anchor.IsZero() {
		return now
	}
	if next := anchor.Add(every); next.After(now) {
		return next
	}
	return now
}

// runOnce is one run: a skip, a written snapshot, a run cancelled by the
// shutdown or a failure, each logged as exactly one line. It returns when the
// next run is due.
func (s *backupScheduler) runOnce(ctx context.Context, start time.Time) time.Time {
	res, out, skipped := s.attempt(ctx, start)
	next := s.nextAfter(start, skipped)
	nextS := rfcTime(next)
	switch {
	case skipped:
		s.log.Info("backup skipped", "reason", "an unfinished snapshot under an hour old is in the directory", "next", nextS)
	case res.Result == backupResultOK:
		s.log.Info("backup written", "file", res.File, "bytes", res.Bytes, "kept", out.Kept,
			"removed", out.Removed, "took", s.now().Sub(start).Round(time.Millisecond), "next", nextS)
	case errors.Is(ctx.Err(), context.Canceled):
		// A clean SIGTERM mid-run (run 8): not a failure. The verb removed its
		// own `.part-`, and the status keeps the last real result.
		s.log.Info("backup cancelled", "reason", "the server is shutting down")
	default:
		args := []any{"err", res.Reason, "next", nextS}
		if res.File != "" {
			args = append(args, "file", res.File)
		}
		s.log.Error("backup failed", args...)
	}
	if ctx.Err() != nil {
		return next // shutting down: nothing more is written
	}
	if !skipped {
		last := res
		s.status.Last = &last
	}
	s.status.Next = nextS
	s.saveStatus()
	return next
}

// attempt does the work of one run and turns a panic inside it into a failed
// run. skipped reports the fresh-part rule.
func (s *backupScheduler) attempt(ctx context.Context, start time.Time) (res backupLast, out backupOutcome, skipped bool) {
	res = backupLast{At: rfcTime(start), Result: backupResultFailed}
	defer func() {
		if p := recover(); p != nil {
			res = backupLast{At: rfcTime(start), Result: backupResultFailed, Reason: fmt.Sprintf("panic: %v", p)}
			out, skipped = backupOutcome{}, false
		}
	}()
	dir := s.cfg.BackupDir
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		res.Reason = fmt.Sprintf("BACKUP_DIR %s is not a directory", dir)
		return res, out, false
	}
	if freshPart(dir, start) {
		return res, out, true
	}
	runCtx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	a := &admin{cfg: s.cfg, db: s.db, ctx: runCtx,
		env: adminEnv{stdout: &stdout, stderr: &stderr, now: func() time.Time { return start }}}
	out, code := a.backupInto(dir, s.cfg.BackupKeep)
	res.File, res.Bytes = out.File, out.Bytes
	if code == 0 {
		res.Result = backupResultOK
	} else {
		res.Reason = backupFailureReason(out, code, stderr.String())
	}
	return res, out, false
}

// backupFailureReason words a failed backupInto. Not "the last line of its
// output": when the snapshot is written and rotation cannot remove an older
// file, the verb exits 1 and its last line is the `rotate:` summary on stdout,
// which is not a reason. That case has its own sentence; every other failure
// is the verb's own last stderr line, verbatim.
func backupFailureReason(out backupOutcome, code int, stderr string) string {
	if out.File != "" && out.RemoveErrs > 0 {
		return fmt.Sprintf("the snapshot was written, but %d older file(s) could not be removed", out.RemoveErrs)
	}
	if line := lastLine(stderr); line != "" {
		return line
	}
	return fmt.Sprintf("exit code %d", code)
}

// freshPart reports whether dir holds a `.part-` file the verb's own rule
// says may still be being written.
func freshPart(dir string, now time.Time) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !partRE.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) < stalePartAge {
			return true
		}
	}
	return false
}

// lastLine is the last non-empty line of s, trimmed.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func (s *backupScheduler) saveStatus() {
	if err := writeBackupStatus(s.cfg.DataDir, s.status); err != nil {
		s.log.Warn("backup status not written", "err", err)
	}
}

func rfcTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseRFCTime is rfcTime's inverse for the status file's fields.
func parseRFCTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty")
	}
	return time.Parse(time.RFC3339, s)
}
