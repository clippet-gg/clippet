package main

// admin_objects.go — `clippet admin objects <id> [--count]` (§20 erratum 281):
// what is in THIS INSTANCE'S STORE under one clip's prefix. Since §20 erratum
// 346 it also has the four write-side forms the test harness used a third-party
// `mc` for — `init`, `put`, `rm` and `rm --prefix` — and a read form from the
// bucket's root, `--prefix`; they are at the end of this file.
//
// It is a BUCKET question and nothing else, which decides three things about it.
//
// **It never opens the database.** Like `doctor` and `restore-check` it is
// routed past `db.Open` in runAdminWith, and that is the feature rather than an
// optimisation: the question worth asking most often is "did deleting this clip
// actually remove its objects", and by the time it can be asked the row is gone.
// A verb that needed a row could not answer for the one case it exists for.
//
// **It goes through `store.Open` and `S3_KEY_PREFIX`**, so it answers for
// whatever this instance actually uses — a bucket on MinIO, R2, B2 or S3 with
// any key prefix, or a directory on `STORAGE_BACKEND=fs` — and it is unaffected
// by `MEDIA_DELIVERY`, which is about how bytes reach a viewer and not about
// where they live. That is the whole point: `scripts/e2e.sh` used to list the
// bundled MinIO directly, which stopped being this stack's store the day
// clippet.gg moved to R2, and every prefix assertion in the suite then saw an
// empty bucket and either failed or — worse — passed for the wrong reason.
//
// **It prints keys.** §24.4's exception, for the same reason `orphans` and
// `ls --stat` print them: the object is the subject of the report, the bucket is
// private, and whoever can run this already holds its credentials. A store
// failure prints the store's own sentence and exits 1; no credential, signature
// or webhook is ever in one.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"clippet/internal/config"
	"clippet/internal/ids"
	"clippet/internal/store"
)

func objectsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: clippet admin objects <id> | _clippet [--count]")
	fmt.Fprintln(w, "       clippet admin objects --prefix <prefix> [--count]")
	fmt.Fprintln(w, "       clippet admin objects init")
	fmt.Fprintln(w, "       clippet admin objects put <key>   (the bytes on stdin)")
	fmt.Fprintln(w, "       clippet admin objects rm <key> | rm --prefix e2e-<word>-<epoch>/")
	fmt.Fprintln(w, "  lists what is in the store under clips/<id>/: one `<key> <size> <modified>` line each, then a count.")
	fmt.Fprintln(w, "  It asks the BUCKET, never the database, so it answers for a clip whose row has already gone.")
	fmt.Fprintln(w, "  A <key> or --prefix is from the bucket's root, not under S3_KEY_PREFIX. init creates the bucket if it is missing.")
	fmt.Fprintln(w, "  rm --prefix removes only a test probe's own prefix, never this instance's.")
}

// adminObjects lists one clip's prefix. It takes the configuration rather than
// an *admin because it runs before the database is opened; the store is reached
// through the same admin.store() every other bucket verb uses, so
// STORAGE_OP_TIMEOUT gets the CLI's floor (adminOpTimeout) here too.
func adminObjects(args []string, cfg config.Config, env adminEnv) int {
	if len(args) > 0 {
		switch args[0] {
		case "init":
			return objectsInit(args[1:], cfg, env)
		case "put":
			return objectsPut(args[1:], cfg, env)
		case "rm":
			return objectsRm(args[1:], cfg, env)
		}
	}
	fs := flag.NewFlagSet("clippet admin objects", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	countOnly := fs.Bool("count", false, "print only the number of objects")
	rootPrefix := fs.String("prefix", "", "list from the bucket's root under this prefix instead of one clip's")
	id, err := parseWithID(fs, args)
	if err == nil && *rootPrefix != "" {
		// The root-relative read form (§20 erratum 346): a probe instance's
		// whole key space, or the doctor's `doctor/` scratch prefix, neither of
		// which is a clip the id form could name.
		if id != "" {
			objectsUsage(env.stderr)
			return 2
		}
		if !validRootKey(strings.TrimSuffix(*rootPrefix, "/")) {
			return objectsRefuse(env, "", fmt.Sprintf("%q is not a key%s", *rootPrefix, keyRule))
		}
		return objectsList(cfg, env, *rootPrefix, *countOnly, false)
	}
	// The instance's own prefix (§20 erratum 343) is asked for by its name:
	// `_clippet` is not an id in either grammar (an underscore is in neither
	// alphabet), so the two can never be confused, and its listing carries a
	// heading saying what it is.
	reserved := err == nil && strings.TrimSuffix(id, "/") == strings.TrimSuffix(store.ReservedDir, "/")
	// An id that is not an id is a USAGE error, not `not found`: every other
	// answer this verb can give is a fact about the store, and "0 objects under a
	// prefix that could not exist" would read as one.
	if err != nil || id == "" || (!reserved && !ids.Valid(id)) {
		objectsUsage(env.stderr)
		return 2
	}
	prefix := store.ClipPrefix(cfg.S3.KeyPrefix, id)
	if reserved {
		prefix = store.ReservedPrefix(cfg.S3.KeyPrefix)
	}
	return objectsList(cfg, env, prefix, *countOnly, reserved)
}

// objectsList is the listing both read forms print: one clip's prefix (or the
// reserved one) and, since §20 erratum 346, any prefix from the bucket's root.
func objectsList(cfg config.Config, env adminEnv, prefix string, countOnly, reserved bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	// db is deliberately nil (see the file comment). Nothing on this path reads
	// it: admin.store() and admin.fail() are the only two methods used.
	a := &admin{cfg: cfg, env: env, ctx: ctx}
	st, err := a.store()
	if err != nil {
		return a.fail("objects", err)
	}
	objs, err := st.List(ctx, prefix)
	if err != nil {
		return a.fail("objects", err)
	}
	if countOnly {
		fmt.Fprintf(env.stdout, "%d\n", len(objs))
		return 0
	}
	if reserved {
		fmt.Fprintf(env.stdout, "%s (this instance's own, never a clip's: the database snapshot, DEPLOY.md §15)\n", prefix)
	}
	// Sorted HERE rather than trusted from the backend: `fs` sorts its walk and
	// S3 returns keys in lexicographic order, but neither is promised by the
	// Store contract, and a listing two backends order differently is one a
	// reader cannot diff.
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	for _, o := range objs {
		fmt.Fprintf(env.stdout, "%s %d %s\n", o.Key, o.Size, o.LastModified.UTC().Format(time.RFC3339))
	}
	// The count line is printed for an empty prefix too (`0 object(s)`), so a
	// caller can tell "nothing there" from "the command produced no output".
	fmt.Fprintf(env.stdout, "%d object(s)\n", len(objs))
	return 0
}

// --- the write side (§20 erratum 346) ----------------------------------------
//
// Until erratum 346 every write the test harness made into a bucket — creating
// it, planting an object, removing one, tidying a probe instance's whole key
// space — went through `mc` in a one-off container: a third-party client whose
// upstream left, pinned by digest beside the S3 server. These four forms are
// that list and nothing more, so the harness talks to its bucket only through
// the binary it is testing, with the stack's own credentials and S3_* exactly
// as the app reads them.
//
// THE KEYS ARE ROOT-RELATIVE. The id form above prepends S3_KEY_PREFIX because
// it asks about this instance's clips; these do not, because what the harness
// writes and removes is mostly NOT this instance's: a probe container is given
// `-e S3_KEY_PREFIX=e2e-<word>-<epoch>/`, which replaces the stack's prefix
// rather than nesting under it, so its objects sit BESIDE the stack's key space.
// A caller that means the stack's own space writes the prefix itself.
//
// EVERY REFUSAL COMES BEFORE THE STORE IS OPENED, so a refused command has
// touched nothing; a refusal is exit 2 and one line on stderr saying why.

// keyRule is the tail of the one sentence a malformed key is refused with.
const keyRule = " (letters, digits and . _ - /; no leading or trailing slash, no empty, . or .. part)"

// validRootKey: a key in the alphabet the app itself writes ([A-Za-z0-9._-]
// and `/` between parts), with no empty, `.` or `..` part. It is stricter than
// either backend needs, on purpose: a key the app could never have written is
// not one this verb has any business with, and `fs` maps a key onto a path.
func validRootKey(k string) bool {
	if k == "" || len(k) > 1024 {
		return false
	}
	for _, part := range strings.Split(k, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}

func objectsRefuse(env adminEnv, verb, why string) int {
	fmt.Fprintf(env.stderr, "admin objects%s: refused: %s\n", verb, why)
	return 2
}

// objectsStore opens the store the way the read form does: no database, the
// CLI's STORAGE_OP_TIMEOUT floor, the whole invocation under adminTimeout.
func objectsStore(cfg config.Config, env adminEnv) (*admin, store.Store, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	a := &admin{cfg: cfg, env: env, ctx: ctx}
	st, err := a.store()
	return a, st, cancel, err
}

// objectsInit creates the bucket when it is missing — what the harness's
// `bucket-init` container did with `mc mb --ignore-existing`. It sets no
// policy: a new bucket is private, and an existing one is left as it is. On
// `fs` there is no bucket, and it says so rather than failing.
func objectsInit(args []string, cfg config.Config, env adminEnv) int {
	if len(args) != 0 {
		objectsUsage(env.stderr)
		return 2
	}
	if cfg.StorageBackend == config.BackendFS {
		fmt.Fprintln(env.stdout, "nothing to create: this instance keeps its media in a directory, not a bucket")
		return 0
	}
	a, st, cancel, err := objectsStore(cfg, env)
	defer cancel()
	if err != nil {
		return a.fail("objects init", err)
	}
	bm, ok := st.(store.BucketMaker)
	if !ok {
		fmt.Fprintln(env.stdout, "nothing to create: this instance keeps its media in a directory, not a bucket")
		return 0
	}
	created, err := bm.EnsureBucket(a.ctx)
	if err != nil {
		return a.fail("objects init", err)
	}
	if created {
		fmt.Fprintf(env.stdout, "bucket %s created\n", cfg.S3.Bucket)
	} else {
		fmt.Fprintf(env.stdout, "bucket %s already exists\n", cfg.S3.Bucket)
	}
	return 0
}

// objectsPut stores stdin under one root-relative key (an existing object is
// replaced). The bytes are spooled to a temporary file first because the
// Store's write is PutFile: it hashes and sizes a file, and a pipe has no size.
func objectsPut(args []string, cfg config.Config, env adminEnv) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		objectsUsage(env.stderr)
		return 2
	}
	key := args[0]
	if !validRootKey(key) {
		return objectsRefuse(env, " put", fmt.Sprintf("%q is not a key%s", key, keyRule))
	}
	if env.stdin == nil {
		return objectsRefuse(env, " put", "there is no stdin to read the object from")
	}
	tmp, err := os.CreateTemp("", "clippet-objects-put-*")
	if err != nil {
		fmt.Fprintf(env.stderr, "admin objects put: %v\n", err)
		return 1
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, env.stdin)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "admin objects put: reading stdin: %v\n", err)
		return 1
	}
	a, st, cancel, err := objectsStore(cfg, env)
	defer cancel()
	if err != nil {
		return a.fail("objects put", err)
	}
	res, err := st.PutFile(a.ctx, key, tmp.Name(), "application/octet-stream", nil)
	if err != nil {
		return a.fail("objects put", err)
	}
	fmt.Fprintf(env.stdout, "stored %s %d\n", key, res.Size)
	return 0
}

// probePrefixRE is the harness's own shape for a probe instance's key space,
// exactly as scripts/e2e.sh's store_rm_prefix_ok_ writes it.
var probePrefixRE = regexp.MustCompile(`^e2e-[a-z0-9]+-[0-9]+/$`)

// probePrefixRefusal is why a recursive removal of p must not run, or "".
//
// It is the harness's store_rm_prefix_ok_ moved into the product, for the
// reason that check exists (§20 erratum 290, AUDIT #28): the prefix arrives in
// a variable expanded late, from an EXIT trap, and an empty one is the bucket.
// So the rule is a shape and not a blocklist — only a probe's own minted
// `e2e-<word>-<epoch>/` passes — and on top of it this instance's own key space
// is refused whichever way the two would overlap. With S3_KEY_PREFIX empty that
// space is the app's own top-level names (clips/, _clippet/, doctor/), none of
// which has the shape.
func probePrefixRefusal(p, keyPrefix string) string {
	switch {
	case p == "" || p == "/" || strings.HasPrefix(p, "."):
		return fmt.Sprintf("%q is empty, the root or a dot-path, which is the whole bucket", p)
	case !probePrefixRE.MatchString(p):
		return fmt.Sprintf("%q is not a test probe's prefix (e2e-<word>-<epoch>/)", p)
	case keyPrefix != "" && (strings.HasPrefix(p, keyPrefix) || strings.HasPrefix(keyPrefix, p)):
		return fmt.Sprintf("%q overlaps this instance's own key space (S3_KEY_PREFIX=%s)", p, keyPrefix)
	}
	return ""
}

// objectsRm removes one root-relative key, or — with --prefix — every object
// under a test probe's own prefix. A key that is not there is not an error
// (the Store's DeleteKey contract); the removal is by key, so it does not
// depend on a listing that may not show a just-written object yet (§20
// erratum 305).
func objectsRm(args []string, cfg config.Config, env adminEnv) int {
	fs := flag.NewFlagSet("clippet admin objects rm", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	prefix := fs.String("prefix", "", "remove every object under this test probe's prefix")
	key, err := parseWithID(fs, args)
	if err != nil {
		objectsUsage(env.stderr)
		return 2
	}
	prefixSet := false
	fs.Visit(func(f *flag.Flag) { prefixSet = prefixSet || f.Name == "prefix" })
	if prefixSet {
		if key != "" {
			objectsUsage(env.stderr)
			return 2
		}
		if why := probePrefixRefusal(*prefix, cfg.S3.KeyPrefix); why != "" {
			return objectsRefuse(env, " rm --prefix", why)
		}
		a, st, cancel, err := objectsStore(cfg, env)
		defer cancel()
		if err != nil {
			return a.fail("objects rm", err)
		}
		n, err := st.DeletePrefix(a.ctx, *prefix)
		if err != nil {
			fmt.Fprintf(env.stdout, "removed %d object(s) under %s before the error below\n", n, *prefix)
			return a.fail("objects rm", err)
		}
		fmt.Fprintf(env.stdout, "removed %d object(s) under %s\n", n, *prefix)
		return 0
	}
	if key == "" {
		objectsUsage(env.stderr)
		return 2
	}
	if !validRootKey(key) {
		return objectsRefuse(env, " rm", fmt.Sprintf("%q is not a key%s", key, keyRule))
	}
	a, st, cancel, err := objectsStore(cfg, env)
	defer cancel()
	if err != nil {
		return a.fail("objects rm", err)
	}
	if err := st.DeleteKey(a.ctx, key); err != nil {
		return a.fail("objects rm", err)
	}
	fmt.Fprintf(env.stdout, "removed %s\n", key)
	return 0
}
