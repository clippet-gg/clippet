package main

// admin_backup_dir.go — `clippet admin backup <dir> [--rotate N]` (ROADMAP
// #14, DESIGN.md §20 erratum 149).
//
// `admin backup <path>` is deliberately non-destructive and DEPLOY §15.5
// accepts the consequences: keep a month of snapshots, and hunt for stranded
// `.part-` files by hand. This makes the cron line safe BY CONSTRUCTION rather
// than by an operator remembering to prune — and it does so without giving the
// command a way to delete a file it did not itself create.
//
// The whole safety argument is the NAME. When the argument is a directory the
// command mints `clippet-<RFC3339 basic UTC>.db`, a shape that sorts
// lexicographically in time order and that nothing else writes; rotation
// considers exactly the files matching that shape, plus the `.part-` files
// this command's own interrupted runs leave beside them. Every other file in
// the directory — a hand-made copy, a `README`, somebody else's backup — is
// invisible to it.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// snapshotStamp is the UTC timestamp format in a minted snapshot name. It is
// RFC 3339 with the separators removed: legal in a filename on every platform,
// unambiguous, and lexicographically ordered, which is what lets rotation sort
// by name instead of trusting mtimes that a copy would rewrite.
const snapshotStamp = "20060102T150405Z"

// snapshotRE matches exactly the names backupInto mints, and nothing else.
var snapshotRE = regexp.MustCompile(`^clippet-\d{8}T\d{6}Z\.db$`)

// partRE matches the `.part-…` file an interrupted snapshot leaves beside its
// destination — again only for names this command mints.
var partRE = regexp.MustCompile(`^clippet-\d{8}T\d{6}Z\.db\.part-[A-Za-z0-9]+$`)

// stalePartAge is how old a `.part-` file must be before rotation removes it.
// An hour is far longer than a `VACUUM INTO` of any database this design
// targets, so a file this old belongs to a run that is over — and a run in
// flight is never touched.
const stalePartAge = time.Hour

// backupOutcome is what backupInto did, for a caller that is not a terminal:
// the server's timed backups (§20 erratum 321) log it and record it, while
// the verb prints its lines and ignores it. File is set once the snapshot is
// in place; RemoveErrs counts the removals rotation could not make.
type backupOutcome struct {
	File                      string
	Bytes                     int64
	Kept, Removed, StaleParts int
	RemoveErrs                int
}

// backupInto writes a dated snapshot into dir and, when rotate > 0, keeps the
// newest `rotate` of them. It prints what it kept and what it removed, one
// fact per line, and exits 1 if any removal failed (the snapshot itself is
// already safely on disk by then). The verb and the server's timed backups
// both call it: there is one implementation.
func (a *admin) backupInto(dir string, rotate int) (backupOutcome, int) {
	var out backupOutcome
	now := a.env.now().UTC()
	name := "clippet-" + now.Format(snapshotStamp) + ".db"
	dst := filepath.Join(dir, name)
	if _, err := os.Lstat(dst); err == nil {
		// Two snapshots in the same second. The "never overwrite" rule of
		// `admin backup` is not weakened here just because the name is
		// derived: the previous second's file is a real backup.
		//
		// The line carries the remedy because a human who runs the command
		// twice in a row reads a bare refusal as something having broken,
		// when what happened is that both runs asked for the same name and
		// the first one already has it. The stamp is NOT given sub-second
		// precision to make this go away: snapshotRE/partRE and the
		// sort-by-name rotation contract are built on the current shape.
		// `already exists` stays in the sentence -- the ops test greps it.
		fmt.Fprintf(a.env.stderr, "admin backup: %s already exists (a snapshot was already taken this second); nothing was overwritten - run it again in a second, or pass an explicit filename\n", dst)
		return out, 1
	}
	size, code := a.writeSnapshot(dst)
	if code != 0 {
		return out, code
	}
	out.File, out.Bytes = dst, size
	if code := a.reportBackup(dst, size); code != 0 {
		return out, code
	}
	if rotate <= 0 {
		fmt.Fprintf(a.env.stdout, "rotate:  off (--rotate N keeps the newest N of this directory's clippet-*.db snapshots)\n")
		return out, 0
	}
	return out, a.rotateSnapshots(dir, name, rotate, now, &out)
}

// rotateSnapshots keeps the newest `keep` minted snapshots in dir and removes
// the rest, plus any stale `.part-` file of its own. `justWritten` is never
// removed, whatever the sort says — a rotation that could delete the backup it
// has just taken is worse than no rotation.
func (a *admin) rotateSnapshots(dir, justWritten string, keep int, now time.Time, out *backupOutcome) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return a.fail("backup rotate", err)
	}
	var snapshots, parts []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch {
		case snapshotRE.MatchString(e.Name()):
			snapshots = append(snapshots, e.Name())
		case partRE.MatchString(e.Name()):
			parts = append(parts, e.Name())
		}
	}
	// Newest first. The name IS the timestamp, so this is a time order that no
	// copy, touch or restore can scramble.
	sort.Sort(sort.Reverse(sort.StringSlice(snapshots)))
	sort.Strings(parts)

	errs := 0
	kept, removed := 0, 0
	for i, name := range snapshots {
		if i < keep || name == justWritten {
			kept++
			fmt.Fprintf(a.env.stdout, "rotate:  keep   %s\n", filepath.Join(dir, name))
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			errs++
			fmt.Fprintf(a.env.stdout, "rotate:  FAILED %s: %v\n", filepath.Join(dir, name), err)
			continue
		}
		removed++
		fmt.Fprintf(a.env.stdout, "rotate:  remove %s\n", filepath.Join(dir, name))
	}
	staleParts := 0
	for _, name := range parts {
		full := filepath.Join(dir, name)
		fi, err := os.Stat(full)
		if err != nil {
			continue // it went away on its own; nothing to report
		}
		if now.Sub(fi.ModTime()) < stalePartAge {
			fmt.Fprintf(a.env.stdout, "rotate:  keep   %s (an interrupted snapshot younger than %s; a run may still be writing it)\n", full, stalePartAge)
			continue
		}
		if err := os.Remove(full); err != nil {
			errs++
			fmt.Fprintf(a.env.stdout, "rotate:  FAILED %s: %v\n", full, err)
			continue
		}
		staleParts++
		fmt.Fprintf(a.env.stdout, "rotate:  remove %s (a stranded partial snapshot)\n", full)
	}
	other := len(mustNotTouch(dir))
	fmt.Fprintf(a.env.stdout, "rotate:  keep=%d removed=%d stale_parts=%d untouched=%d (--rotate %d)\n",
		kept, removed, staleParts, other, keep)
	out.Kept, out.Removed, out.StaleParts, out.RemoveErrs = kept, removed, staleParts, errs
	if errs > 0 {
		return 1
	}
	return 0
}

// mustNotTouch lists the files in dir that rotation is not allowed to consider
// — everything whose name this command did not mint. It exists so the summary
// line can SAY how many files were left alone, which is the property the
// command is claiming.
func mustNotTouch(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if snapshotRE.MatchString(n) || partRE.MatchString(n) {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// humanBytes renders a byte count for a human, keeping the raw number's own
// column elsewhere on the line. It is the one renderer `admin stats`,
// `admin doctor` and this file share.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
