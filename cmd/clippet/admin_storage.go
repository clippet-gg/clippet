package main

// admin_storage.go — `clippet admin storage migrate` (ROADMAP #37, DESIGN.md
// §20 erratum 276).
//
//	clippet admin storage migrate --to fs|s3 [--dry-run] [--manifest <path>]
//	                              [--stop-file <path>] [--max-bytes N]
//
// The one-shot copy that lets an operator move off MinIO onto a mounted share,
// or back onto a bucket, without losing a clip. ROADMAP #37's non-goals hold:
// no multi-node story, no replication, no "both at once" mode — one copy that
// verifies, and the operator flips `STORAGE_BACKEND` afterwards.
//
// Three rules it keeps, and the reasons are the project's own:
//
//   - THE SOURCE IS NEVER TOUCHED. Not one delete, not on success, not on a
//     re-run. The rule that a destructive action needs its exact target
//     printed and an explicit yes is satisfied here by there being no destructive action at
//     all: what is left over after a migration is the operator's to remove,
//     with the old store still configured and `admin stats` still able to
//     count it.
//   - THE PLAN IS PRINTED BEFORE ANY BYTE MOVES — clips, objects, bytes, both
//     stores named — and `--dry-run` prints exactly that and stops. An
//     operator should never learn the size of an operation from watching it.
//   - EVERY OBJECT IS VERIFIED at the destination: the size it reports back,
//     and the sha256 of the WHOLE object read back through the destination
//     store, compared with what the source actually delivered. A copy that
//     says "done" without having read anything back has proved the API works,
//     not that the clip survived — and one verified by its first few KiB has
//     proved that the file starts correctly. Since §20 erratum 337 the
//     read-back is the FALLBACK: a bucket whose ETag is the one predicted
//     from the bytes that were hashed (R2's and AWS's documented MD5 rule)
//     has proved the copy without a read, and the verdict line says which
//     proof it was — `(etag proof)` or `(read back)`.
//
// Since §20 erratum 335 it also copies bucket to bucket — `--to s3` on an `s3`
// instance, with the destination in the seven MIGRATE_S3_* variables — keeps a
// manifest of what it verified so a re-run hashes only what is new, and stops
// BETWEEN objects on a stop file or a byte cap (exit 3). The desktop app's
// Storage screen drives it through exactly those three flags.
//
// The relaxation this verb needs — both backends configured at once, which
// `config.Load` refuses on `fs` — is `config.LoadForMigrate`, routed by
// cmd/clippet/main.go on this exact subcommand and reachable from nowhere
// else. `clippet serve` therefore cannot run with a bucket configured beside
// STORAGE_BACKEND=fs, which is what keeps §15's sentence true.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"clippet/internal/config"
	"clippet/internal/store"
)

func (a *admin) storageUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin storage migrate --to fs|s3 [--dry-run] [--manifest <path>] [--stop-file <path>] [--max-bytes N]")
	fmt.Fprintln(a.env.stderr, "  Copies every live clip's objects from the store this instance is configured to use")
	fmt.Fprintln(a.env.stderr, "  to the other one, verifies each copy, and prints a summary. The SOURCE is never")
	fmt.Fprintln(a.env.stderr, "  touched; flip STORAGE_BACKEND yourself when it has finished (DEPLOY.md §32).")
}

func (a *admin) storage(args []string) int {
	if len(args) == 0 || args[0] != "migrate" {
		a.storageUsage()
		return 2
	}
	fs := a.flags("storage migrate")
	to := fs.String("to", "", "the backend to copy INTO: fs|s3")
	dry := fs.Bool("dry-run", false, "print the plan and copy nothing")
	manifest := fs.String("manifest", "", "a file of verified objects, one <key> <size> <sha256> per line (tab-separated); a re-run skips those without hashing")
	stopFile := fs.String("stop-file", "", "stop between objects once this file exists (exit 3)")
	maxBytes := fs.Int64("max-bytes", 0, "stop before the object that would take this run's bytes past N (exit 3)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *maxBytes < 0 {
		a.storageUsage()
		return 2
	}
	target := strings.ToLower(strings.TrimSpace(*to))
	switch target {
	case config.BackendFS, config.BackendS3:
	default:
		fmt.Fprintf(a.env.stderr, "admin storage migrate: --to must be %s or %s, got %q\n", config.BackendFS, config.BackendS3, *to)
		a.storageUsage()
		return 2
	}
	// Bucket to bucket (§20 erratum 335): `--to s3` on an `s3` instance is a
	// move when the MIGRATE_S3_* variables name the destination, today's
	// refusal when none is set, and a list of what is missing when some are.
	b2b := target == config.BackendS3 && a.cfg.StorageBackend == config.BackendS3 && a.cfg.MigrateS3Set()
	if target == a.cfg.StorageBackend && !b2b {
		fmt.Fprintf(a.env.stderr, "admin storage migrate: --to %s is the store this instance already uses (STORAGE_BACKEND=%s); there is nothing to copy\n",
			target, a.cfg.StorageBackend)
		return 2
	}
	if b2b {
		if missing := a.cfg.MigrateS3Missing(); len(missing) > 0 {
			fmt.Fprintf(a.env.stderr, "admin storage migrate: --to s3 needs %s in this environment; %s unset\n",
				strings.Join(config.MigrateS3Vars, ", "), strings.Join(missing, ", "))
			return 2
		}
	}
	job := migrateJob{
		target: target, dry: *dry, b2b: b2b,
		manifestPath: *manifest, stopFile: *stopFile, maxBytes: *maxBytes,
		srcPrefix: a.cfg.S3.KeyPrefix, dstPrefix: a.cfg.S3.KeyPrefix,
	}
	if b2b {
		job.dstPrefix = a.cfg.MigrateS3.KeyPrefix
	}
	if job.manifestPath != "" {
		m, err := readMigrateManifest(job.manifestPath)
		if err != nil {
			fmt.Fprintf(a.env.stderr, "admin storage migrate: --manifest %s: %v\n", job.manifestPath, err)
			return 2
		}
		job.manifest = m
	}
	// NOT the invocation's ten-minute budget (§20 erratum 350). That ceiling is
	// right for a verb that ASKS something and wrong for one that MOVES a
	// library: DEPLOY §32.3's own example, 412 clips and 44.9 GiB, cannot copy
	// and verify in ten minutes, and a context that expired half way through
	// turned every remaining object into `did NOT arrive verified` with nothing
	// wrong anywhere. `admin import` was given its own context for the same
	// reason (§20 erratum 307). The run itself has no deadline — the stop file
	// and the byte cap stop it, between objects — and every object carries one
	// of its own (migrateObjectBudget), so a hung object fails alone and the
	// next one starts.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.ctx = ctx
	src, dst, code := a.migrateStores(job)
	if code != 0 {
		return code
	}
	return a.migrateRun(src, dst, &job)
}

// migrateObjectBudget is the deadline ONE object's copy and verify runs under
// (§20 erratum 350): STORAGE_TRANSFER_MAX, the ceiling the store already gives
// one transfer (six hours by default), so the object it fits on a bucket also
// fits here — and a directory, whose transfers have no ceiling of their own
// (DEPLOY §32.2), gets that one. A value of 0 there means "no cap" and is
// honoured as that.
func (a *admin) migrateObjectBudget() time.Duration {
	if a.env.objectTimeout > 0 {
		return a.env.objectTimeout
	}
	return a.cfg.S3.TransferMax
}

// migrateObjectContext is the context migrateOne runs under.
func (a *admin) migrateObjectContext() (context.Context, context.CancelFunc) {
	if d := a.migrateObjectBudget(); d > 0 {
		return context.WithTimeout(a.ctx, d)
	}
	return context.WithCancel(a.ctx)
}

// migrateJob is one run's whole shape: where to, what the flags asked for, and
// the key spaces on either side (§20 erratum 335).
type migrateJob struct {
	target string
	dry    bool
	// b2b is bucket to bucket: the destination is MIGRATE_S3_*, not S3_*.
	b2b bool
	// srcPrefix and dstPrefix are the two key spaces. They differ only on a
	// bucket-to-bucket run with a MIGRATE_S3_KEY_PREFIX of its own.
	srcPrefix, dstPrefix string

	manifestPath string
	manifest     map[string]migrateManifestEntry
	manifestOut  *os.File
	stopFile     string
	maxBytes     int64
}

// dstKey is where a source key lands at the destination.
func (j *migrateJob) dstKey(key string) string {
	if j.srcPrefix == j.dstPrefix {
		return key
	}
	return j.dstPrefix + strings.TrimPrefix(key, j.srcPrefix)
}

// migrateManifestEntry is one verified object of an earlier run.
type migrateManifestEntry struct {
	size int64
	sha  string
}

// emptySHA256 is the sha256 of zero bytes: what a manifest line says for an
// empty object, which migrateHash reports as "".
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// readMigrateManifest reads --manifest. A missing file is an empty manifest;
// an unreadable file or a malformed line is an error — never a silent trust.
// A line for a key the plan no longer holds is simply never asked about.
func readMigrateManifest(p string) (map[string]migrateManifestEntry, error) {
	m := map[string]migrateManifestEntry{}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || parts[0] == "" {
			return nil, fmt.Errorf("line %d is not <key> <size> <sha256>, tab-separated", n)
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("line %d: size %q", n, parts[1])
		}
		if len(parts[2]) != 64 || strings.Trim(parts[2], "0123456789abcdef") != "" {
			return nil, fmt.Errorf("line %d: sha256 %q", n, parts[2])
		}
		m[parts[0]] = migrateManifestEntry{size: size, sha: parts[2]}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// manifestAppend records one verified object.
func (j *migrateJob) manifestAppend(key string, size int64, sha string) error {
	if j.manifestOut == nil {
		return nil
	}
	if sha == "" && size == 0 {
		sha = emptySHA256
	}
	_, err := j.manifestOut.WriteString(key + "\t" + strconv.FormatInt(size, 10) + "\t" + sha + "\n")
	return err
}

// stopReason is what the run reads BETWEEN objects: the stop file first, then
// whether the next object would take this run past --max-bytes. "" goes on.
func (j *migrateJob) stopReason(counted, next int64) string {
	if j.stopFile != "" {
		if _, err := os.Stat(j.stopFile); err == nil {
			return "stop-file"
		}
	}
	if j.maxBytes > 0 && counted+next > j.maxBytes {
		return "max-bytes"
	}
	return ""
}

// migrateStores builds the pair. The CONFIGURED backend is the source and the
// other one is the destination, both out of the same `.env` — which is the
// whole reason `LoadForMigrate` exists, and the simplest honest shape: an
// operator who has to hand credentials to a command line has already lost them
// to their shell history (§20 erratum 109's rule, applied to a bucket key).
//
// Bucket to bucket the destination is the MIGRATE_S3_* bucket, on S3's clock
// (§20 erratum 335).
func (a *admin) migrateStores(j migrateJob) (src, dst store.Store, code int) {
	target := j.target
	cfg := withAdminTimeout(a.cfg)
	src, err := a.env.openStore(cfg)
	if err != nil {
		return nil, nil, a.fail("storage migrate", fmt.Errorf("opening the source (%s): %w", a.cfg.StorageBackend, err))
	}
	other := cfg
	other.StorageBackend = target
	if j.b2b {
		m := cfg.MigrateS3
		m.OpTimeout, m.IdleTimeout, m.TransferMax = cfg.S3.OpTimeout, cfg.S3.IdleTimeout, cfg.S3.TransferMax
		m.UploadThreads = cfg.S3.UploadThreads
		other.S3 = m
		other.MigrateS3 = config.S3Config{}
		dst, err = a.env.openStore(other)
		if err != nil {
			return nil, nil, a.fail("storage migrate", fmt.Errorf("opening the destination (%s): %w", target, err))
		}
		return src, dst, 0
	}
	if missing := migrateMissing(other); len(missing) > 0 {
		fmt.Fprintf(a.env.stderr, "admin storage migrate: --to %s needs %s in this environment; %s unset\n",
			target, strings.Join(migrateNeeds(target), ", "), strings.Join(missing, ", "))
		fmt.Fprintf(a.env.stderr, "  Set them beside the ones you already have — this one command is allowed to see both stores (DEPLOY.md §32).\n")
		return nil, nil, 2
	}
	dst, err = a.env.openStore(other)
	if err != nil {
		return nil, nil, a.fail("storage migrate", fmt.Errorf("opening the destination (%s): %w", target, err))
	}
	return src, dst, 0
}

// migrateNeeds names what a target backend is configured by.
func migrateNeeds(target string) []string {
	if target == config.BackendFS {
		return []string{"STORAGE_DIR"}
	}
	return config.S3Vars
}

// migrateMissing reports which of those are empty in cfg.
func migrateMissing(cfg config.Config) []string {
	var missing []string
	if cfg.StorageBackend == config.BackendFS {
		if cfg.StorageDir == "" {
			missing = append(missing, "STORAGE_DIR")
		}
		return missing
	}
	for name, v := range map[string]string{
		"S3_ENDPOINT": cfg.S3.Endpoint, "S3_BUCKET": cfg.S3.Bucket,
		"S3_ACCESS_KEY": cfg.S3.AccessKey, "S3_SECRET_KEY": cfg.S3.SecretKey,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// migratePlan is what the preflight found: one entry per live clip.
type migratePlan struct {
	clips  []migrateClip
	objs   int
	bytes  int64
	prefix string
}

type migrateClip struct {
	id    string
	state string
	objs  []store.ObjectInfo
}

// migrateRun does the preflight, prints the plan and — unless --dry-run —
// copies and verifies.
func (a *admin) migrateRun(src, dst store.Store, j *migrateJob) int {
	target, dry := j.target, j.dry
	w := a.env.stdout
	if err := src.Ping(a.ctx); err != nil {
		return a.fail("storage migrate", fmt.Errorf("the source store (%s) does not answer: %w", a.cfg.StorageBackend, err))
	}
	if err := dst.Ping(a.ctx); err != nil {
		return a.fail("storage migrate", fmt.Errorf("the destination store (%s) does not answer: %w", target, err))
	}
	plan, code := a.migratePreflight(src)
	if code != 0 {
		return code
	}
	fmt.Fprintf(w, "migrate: from %s to %s\n", a.migrateWhere(a.cfg.StorageBackend, false), a.migrateWhere(target, j.b2b))
	fmt.Fprintf(w, "migrate: plan clips=%d objects=%d bytes=%d (%s) key_prefix=%s\n",
		len(plan.clips), plan.objs, plan.bytes, humanBytes(plan.bytes), orNoPrefix(plan.prefix))
	for _, c := range plan.clips {
		var b int64
		for _, o := range c.objs {
			b += o.Size
		}
		fmt.Fprintf(w, "migrate: plan %s state=%s objects=%d bytes=%d\n", c.id, c.state, len(c.objs), b)
	}
	fmt.Fprintf(w, "migrate: the source is NEVER written to or deleted from; flip STORAGE_BACKEND yourself when this has finished\n")
	if dry {
		fmt.Fprintf(w, "migrate: --dry-run: nothing was copied\n")
		return 0
	}
	if plan.objs == 0 {
		fmt.Fprintf(w, "migrate: done copied=0 skipped=0 bytes=0 failed=0 (there was nothing to copy)\n")
		return a.migrateFinish(target, 0)
	}
	if j.manifestPath != "" {
		f, err := os.OpenFile(j.manifestPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(a.env.stderr, "admin storage migrate: --manifest %s: %v\n", j.manifestPath, err)
			return 2
		}
		defer f.Close()
		j.manifestOut = f
	}
	copied, skipped, failed, attempted := 0, 0, 0, 0
	var moved, counted int64
	for _, c := range plan.clips {
		for _, o := range c.objs {
			// Read BETWEEN objects: after one finishes, before the next
			// starts. Nothing is removed and no marker is written (§20
			// erratum 335).
			if reason := j.stopReason(counted, o.Size); reason != "" {
				fmt.Fprintf(w, "migrate: stopped reason=%s copied=%d skipped=%d bytes=%d (%s) failed=%d remaining=%d\n",
					reason, copied, skipped, moved, humanBytes(moved), failed, plan.objs-attempted)
				return 3
			}
			attempted++
			octx, ocancel := a.migrateObjectContext()
			outcome, sha := a.migrateOne(octx, src, dst, j, o)
			ocancel()
			switch outcome {
			case migrateCopied:
				copied++
				moved += o.Size
				counted += o.Size
			case migrateSkipped, migrateSkippedManifest:
				skipped++
				counted += o.Size
			default:
				failed++
			}
			if outcome == migrateCopied || outcome == migrateSkipped {
				if err := j.manifestAppend(o.Key, o.Size, sha); err != nil {
					fmt.Fprintf(a.env.stderr, "admin storage migrate: --manifest %s: %v\n", j.manifestPath, err)
					return 2
				}
			}
		}
	}
	fmt.Fprintf(w, "migrate: done copied=%d skipped=%d bytes=%d (%s) failed=%d\n",
		copied, skipped, moved, humanBytes(moved), failed)
	if failed > 0 {
		fmt.Fprintf(w, "migrate: %d object(s) did NOT arrive verified — do not flip STORAGE_BACKEND; fix the cause and run this again (it re-copies only what is missing)\n", failed)
		return 1
	}
	return a.migrateFinish(target, copied)
}

// migrateFinish writes the store marker when the destination is a directory,
// so the very next `admin doctor` on the flipped instance recognises it (§20
// erratum 276).
func (a *admin) migrateFinish(target string, copied int) int {
	if target != config.BackendFS {
		return 0
	}
	if err := store.WriteMarker(a.cfg.StorageDir); err != nil {
		fmt.Fprintf(a.env.stdout, "migrate: the store marker could not be written in %s (%v); create an empty file named .clippet-store at its root before `clippet serve` (DEPLOY.md §32.4)\n", a.cfg.StorageDir, err)
		return 0
	}
	fmt.Fprintf(a.env.stdout, "migrate: %s written in %s\n", store.MarkerName, a.cfg.StorageDir)
	_ = copied
	return 0
}

// migrateWhere names a store the way an operator reads it. It prints no
// credential — the endpoint and the bucket are already in `.env` and in every
// `doctor` line, the key never is.
//
// migrateS3 names the MIGRATE_S3_* bucket: the destination of a
// bucket-to-bucket run (§20 erratum 335).
func (a *admin) migrateWhere(backend string, migrateS3 bool) string {
	if backend == config.BackendFS {
		return "fs (" + a.cfg.StorageDir + ")"
	}
	if migrateS3 {
		return "s3 (" + a.cfg.MigrateS3.Bucket + " at " + a.cfg.MigrateS3.Endpoint + ")"
	}
	return "s3 (" + a.cfg.S3.Bucket + " at " + a.cfg.S3.Endpoint + ")"
}

// orNoPrefix spells an empty S3_KEY_PREFIX, which is the default: a blank in
// a plan line reads as a missing value rather than as an absent one.
func orNoPrefix(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// migratePreflight lists the source objects of every clip the database still
// has a row for.
//
// It walks the ROWS and lists each clip's own prefix, rather than listing the
// whole of `clips/` once: a row-less prefix is an ORPHAN, `admin orphans` is
// the verb that reports and removes those, and a migration that silently
// carried an instance's garbage to its new home would make the first thing an
// operator does on the new store a clean-up.
func (a *admin) migratePreflight(src store.Store) (migratePlan, int) {
	plan := migratePlan{prefix: a.cfg.S3.KeyPrefix}
	rows, err := a.db.ListAllClipIDsAndKeys(a.ctx)
	if err != nil {
		return plan, a.fail("storage migrate", err)
	}
	ids := make([]string, 0, len(rows))
	states := make(map[string]string, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		states[r.ID] = r.State
	}
	sort.Strings(ids)
	for _, id := range ids {
		objs, err := src.List(a.ctx, store.ClipPrefix(a.cfg.S3.KeyPrefix, id))
		if err != nil {
			return plan, a.fail("storage migrate", fmt.Errorf("listing %s: %w", id, err))
		}
		if len(objs) == 0 {
			continue
		}
		plan.clips = append(plan.clips, migrateClip{id: id, state: states[id], objs: objs})
		plan.objs += len(objs)
		for _, o := range objs {
			plan.bytes += o.Size
		}
	}
	return plan, 0
}

type migrateOutcome int

const (
	migrateCopied migrateOutcome = iota
	migrateSkipped
	// migrateSkippedManifest is a skip on the manifest's word and the
	// destination's size, with nothing hashed (§20 erratum 335).
	migrateSkippedManifest
	migrateFailed
)

// migrateOne copies and verifies one object. It goes through a scratch file
// rather than piping GetRange into a PUT because that is what the Store
// contract offers on both sides — Download and PutFile — and because PutFile's
// own sha256 of the scratch copy is then the hash of exactly the bytes the
// source delivered, which is what the destination is checked against.
//
// Since §20 erratum 337 an object is VERIFIED one of two ways, and every
// verdict line says which:
//
//   - `(etag proof)`: the destination's ETag is the S3-shaped one predicted
//     from the very bytes that were hashed (store.ETagHasher, in the same pass
//     as the sha256). Nothing is read back. This is R2's and AWS's documented
//     ETag rule; PutFile also sends a Content-MD5 per part, so a part that
//     arrived wrong was refused at write time.
//   - `(read back)`: anything else — a provider whose ETag is shaped some other
//     way, a part size that differs from ours, or a directory, which has no
//     ETag at all (erratum 276) — is read back IN FULL through the destination
//     and hashed, exactly as before. The rule is provider-agnostic on purpose:
//     a provider this project has no document for simply never matches, which
//     is the safe side.
//
// The sha256 it returns is the verified hash, for the manifest; "" on a
// failure, a manifest skip, or a zero-byte skip.
func (a *admin) migrateOne(ctx context.Context, src, dst store.Store, j *migrateJob, o store.ObjectInfo) (migrateOutcome, string) {
	w := a.env.stdout
	dk := j.dstKey(o.Key)
	// In the manifest at this size, and the destination says the same size:
	// skipped WITHOUT hashing — the manifest is the record of an earlier
	// verified copy (§20 erratum 335).
	if m, ok := j.manifest[o.Key]; ok && m.size == o.Size {
		if info, err := dst.Stat(ctx, dk); err == nil && info.Size == o.Size {
			fmt.Fprintf(w, "migrate: skip %s size=%d (in the manifest)\n", o.Key, o.Size)
			return migrateSkippedManifest, ""
		}
	}
	// Already there at the same size? Verify it — the WHOLE object, not a
	// sample — and skip it only if it really is the same file. A re-run after a
	// failure must be cheap, and "cheap" must never mean "unchecked".
	//
	// The SOURCE is read once, for the manifest's sha256 and the expected ETag
	// together; the destination is read only when its ETag does not prove the
	// copy (§20 erratum 337).
	//
	// A mismatch here falls THROUGH to the copy below rather than failing. An
	// object at the destination that does not match the source is exactly what
	// a migration exists to fix, and the summary's own remedy ("run this
	// again") has to be true: before this, a same-size corrupt copy was a
	// permanent failure that no re-run could clear.
	if info, err := dst.Stat(ctx, dk); err == nil && info.Size == o.Size {
		srcSHA, srcETag, serr := a.migrateHash(ctx, src, o.Key, o.Size)
		if serr != nil {
			fmt.Fprintf(w, "migrate: FAILED %s: re-reading it from the source: %v\n", o.Key, serr)
			return migrateFailed, ""
		}
		if store.ETagMatches(srcETag, info.ETag) {
			fmt.Fprintf(w, "migrate: skip %s size=%d (already at the destination, verified sha256=%s) (etag proof)\n", o.Key, o.Size, srcSHA)
			return migrateSkipped, srcSHA
		}
		if verr := a.migrateVerify(ctx, dst, dk, o.Size, srcSHA); verr != nil {
			fmt.Fprintf(w, "migrate: %s is at the destination but does not match the source (%v); copying it again\n", o.Key, verr)
		} else {
			fmt.Fprintf(w, "migrate: skip %s size=%d (already at the destination, verified sha256=%s) (read back)\n", o.Key, o.Size, srcSHA)
			return migrateSkipped, srcSHA
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(w, "migrate: FAILED %s: the destination could not be asked about it: %v\n", o.Key, err)
		return migrateFailed, ""
	}

	tmp, err := os.CreateTemp(a.cfg.ScratchDir, ".clippet-migrate-*")
	if err != nil {
		fmt.Fprintf(w, "migrate: FAILED %s: scratch file: %v\n", o.Key, err)
		return migrateFailed, ""
	}
	name := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(name)
	// Progress per byte on the leg that crosses the network (§20 erratum
	// 337): the WRITE into a bucket, or the READ out of one into a directory.
	// Bucket to bucket it is the write. One leg per object, so the bytes on an
	// object's lines only ever grow.
	prog := newMigrateProgress(w, o.Key, o.Size, a.migrateNow())
	var readProg, writeProg func(done, total int64)
	if j.target == config.BackendFS {
		readProg = prog.update
	} else {
		writeProg = prog.update
	}
	if err := src.Download(ctx, o.Key, name, readProg); err != nil {
		fmt.Fprintf(w, "migrate: FAILED %s: reading it from the source: %v\n", o.Key, err)
		return migrateFailed, ""
	}
	if readProg != nil {
		prog.finish()
	}
	res, err := dst.PutFile(ctx, dk, name, migrateContentType(o.Key), writeProg)
	if err != nil {
		fmt.Fprintf(w, "migrate: FAILED %s: writing it to the destination: %v\n", o.Key, err)
		return migrateFailed, ""
	}
	if writeProg != nil {
		prog.finish()
	}
	if res.Size != o.Size {
		fmt.Fprintf(w, "migrate: FAILED %s: the destination stored %d bytes of %d\n", o.Key, res.Size, o.Size)
		return migrateFailed, ""
	}
	// The ETag proof: what the destination reports for the key NOW, against
	// the ETag predicted from the scratch copy's bytes in the pass that took
	// res.SHA256 (§20 erratum 337).
	if info, err := dst.Stat(ctx, dk); err == nil && info.Size == o.Size && store.ETagMatches(res.ExpectedETag, info.ETag) {
		fmt.Fprintf(w, "migrate: copied %s size=%d sha256=%s (etag proof)\n", o.Key, res.Size, res.SHA256)
		return migrateCopied, res.SHA256
	}
	// res.SHA256 is the hash of the scratch copy, which is byte for byte what
	// the source delivered — so checking the destination against it costs no
	// second read of the source.
	if err := a.migrateVerify(ctx, dst, dk, o.Size, res.SHA256); err != nil {
		fmt.Fprintf(w, "migrate: FAILED %s: %v\n", o.Key, err)
		return migrateFailed, ""
	}
	fmt.Fprintf(w, "migrate: copied %s size=%d sha256=%s (read back)\n", o.Key, res.Size, res.SHA256)
	return migrateCopied, res.SHA256
}

// migrateNow is the progress line's clock: the admin's own (a test pins it),
// else the wall clock.
func (a *admin) migrateNow() func() time.Time {
	if a.env.now != nil {
		return a.env.now
	}
	return time.Now
}

// migrateProgressEvery is how often an object's progress line may print.
const migrateProgressEvery = time.Second

// migrateProgress prints `migrate: progress <key> <bytes>/<size>` for one
// object (§20 erratum 337): at most once per migrateProgressEvery while bytes
// move, and once more at the end of the leg with the whole size, unless the
// last line already said it. The desktop app reads these lines; the shape is
// the contract (DEPLOY §32.3).
type migrateProgress struct {
	w    io.Writer
	key  string
	size int64
	now  func() time.Time

	mu      sync.Mutex
	started time.Time
	last    time.Time // when a line last printed; zero = none yet
	printed int64     // the bytes that line said; -1 = none yet
	done    int64
}

func newMigrateProgress(w io.Writer, key string, size int64, now func() time.Time) *migrateProgress {
	return &migrateProgress{w: w, key: key, size: size, now: now, started: now(), printed: -1}
}

// update is the store's progress callback. It may run on a goroutine other
// than the caller's (Store.PutFile's contract), hence the lock.
func (p *migrateProgress) update(done, _ int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if done > p.done {
		p.done = done
	}
	t := p.now()
	since := p.last
	if since.IsZero() {
		since = p.started
	}
	if t.Sub(since) < migrateProgressEvery || p.done == p.printed {
		return
	}
	p.print(p.done, t)
}

// finish prints the end line: the whole size, once.
func (p *migrateProgress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.printed == p.size {
		return
	}
	p.print(p.size, p.now())
}

func (p *migrateProgress) print(n int64, t time.Time) {
	fmt.Fprintf(p.w, "migrate: progress %s %d/%d\n", p.key, n, p.size)
	p.printed, p.last = n, t
}

// migrateVerify reads the object back THROUGH THE DESTINATION STORE, in full,
// and checks its size and its sha256 against what the source holds.
//
// The whole object, not a sample. The first cut of this compared the first
// 64 KiB — enough to catch a truncated or empty copy, and blind to a
// corruption anywhere after it — and "verified" has to mean verified. Since
// §20 erratum 337 it is the FALLBACK: an object whose destination ETag proves
// the copy is not read back at all, and everything else still is.
func (a *admin) migrateVerify(ctx context.Context, dst store.Store, key string, wantSize int64, wantSHA string) error {
	info, err := dst.Stat(ctx, key)
	if err != nil {
		return fmt.Errorf("the destination cannot stat it back: %w", err)
	}
	if info.Size != wantSize {
		return fmt.Errorf("the destination holds %d bytes, the source %d", info.Size, wantSize)
	}
	if wantSize == 0 {
		return nil // a zero-byte object has no satisfiable range on either store
	}
	got, _, err := a.migrateHash(ctx, dst, key, wantSize)
	if err != nil {
		return fmt.Errorf("reading it back from the destination: %w", err)
	}
	if got != wantSHA {
		return fmt.Errorf("sha256 %s at the destination, %s at the source", got, wantSHA)
	}
	return nil
}

// migrateHash streams a whole object out of a store and returns its sha256
// and — in the same pass — the S3-shaped ETag an upload of it at
// store.UploadPartSize should be given (§20 erratum 337). It reads through
// GetRange — the same call the media routes use — so what it hashes is what a
// viewer would be served, not what the filesystem happens to hold beside it.
//
// A zero-byte object is not read (no range is satisfiable): its sha256 is
// reported as "" (the manifest spells it), its ETag as the MD5 of nothing.
func (a *admin) migrateHash(ctx context.Context, st store.Store, key string, size int64) (sha, etag string, err error) {
	eh := store.NewETagHasher(store.UploadPartSize)
	if size == 0 {
		return "", eh.Sum(), nil
	}
	rc, err := st.GetRange(ctx, key, 0, -1)
	if err != nil {
		return "", "", err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, eh), rc)
	if err != nil {
		return "", "", err
	}
	if n != size {
		return "", "", fmt.Errorf("read %d bytes of %d", n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), eh.Sum(), nil
}

// migrateContentType is the Content-Type the destination is handed. It is
// derived from the key's EXTENSION, which keys.go fixes per object kind
// (`.mp4`, `.jpg`, `.gif`, `.json`) — the one key whose extension is the
// uploader's is the original, and the app reads that type out of the database
// rather than out of the store on every route that serves it (§15). On a
// filesystem destination the value is ignored entirely (§20 erratum 276).
func migrateContentType(key string) string {
	ext := strings.TrimPrefix(path.Ext(key), ".")
	if ext == "json" {
		return "application/json"
	}
	if ext == "jpg" || ext == "jpeg" {
		return "image/jpeg"
	}
	if ext == "gif" {
		return "image/gif"
	}
	return store.ContentTypeFor("", ext)
}
