package web

// setup.go — the option table behind `/setup` (ROADMAP #45, DESIGN.md §20
// errata 297-299, 309-311; DEPLOY.md §35).
//
// Pure data and formatting: no I/O, no clock, no database. Everything here is
// a function of the TARGET the person chose, plus exactly one boolean about
// how this instance is reached (§20 erratum 299) — which is what keeps the
// table five rows long instead of five times a matrix.
//
// Three laws hold over every string in this file, and each has a test:
//
//  1. **No URL.** Not in a step, not in a hint, not in a block, not in a
//     comment. `scripts/check.sh` greps `https?://` over every .go and .html
//     under internal/, cmd/ and web/ — comments included — and this file and
//     web/templates/setup.html are both in that set. Endpoints therefore
//     follow one of two placeholder grammars: PASTE-WHOLE for a provider that
//     shows you an endpoint, CONSTRUCTED (`<scheme>://…`) where none exists to
//     copy. Neither spelling matches the grep. Real endpoints, region codes
//     and the provider's own pages live in DEPLOY §35, which is a .md.
//
//     **One exception, since §20 erratum 303, and it is a narrow one.** The
//     Clippet Pass block prints `PUBLIC_BASE_URL` as a whole secure base on
//     the Pass zone — the scheme is not the operator's to choose there, a
//     Pass name is served over TLS by one edge and by nothing else, so a
//     `<scheme>://` placeholder would be a question with one answer and a
//     paste that forgot to answer it would not boot. It is not an endpoint
//     this app ever opens, not a link the page renders, and not a third
//     party's host: it is the value of THIS instance's own base URL, one
//     `.env` line, inside a block. `passScheme` keeps the source clear of the
//     grep the way `scripts/check.sh`'s own message says to — split the
//     scheme — and setup_test.go's golden, which the grep exempts, pins the
//     rendered line as bytes.
//  2. **Every block is TOTAL.** It ASSIGNS every variable that decides the
//     outcome — `COMPOSE_FILE`, `STORAGE_BACKEND`, `MEDIA_DELIVERY`, the
//     pricing, and every `S3_*` this app reads when the backend is a bucket —
//     and never tells anybody to delete a line, nor relies on one being
//     absent. (Until §20 erratum 345 a sixth row, the bundled MinIO, assigned
//     its `S3_*` EMPTY so that compose's `${NAME:-<bundled default>}` handed
//     the bundled values back; that server is gone, and the folder block
//     needs no `S3_*` line because compose.fs.yaml passes them through
//     empty.) `COMPOSE_FILE` is
//     read by compose on the HOST, is never passed into the container and is
//     therefore invisible to this app: it cannot be detected, warned about or
//     checked afterwards. An instruction this product can neither observe nor
//     verify is exactly the kind that gets skipped; an assignment is
//     idempotent and needs no observing. That is what `compose.nobucket.yaml`
//     exists for — an external bucket needed a `COMPOSE_FILE` value of its own
//     to be assigned (§20 erratum 298).
//  3. **No credential, and no data-moving command.** The page has no field to
//     type a key into and prints no `admin storage migrate` in either
//     direction: that command is order-sensitive, needs a bind DEPLOY §32.3
//     puts in a boxed warning, and is refused if it is run after the flip. A
//     page that prints a data-moving command must print the documented one or
//     none; after 2026-09-11, none (§20 erratum 297).

import (
	"strconv"
	"strings"

	"clippet/internal/config"
)

// walksRead is the date somebody last read the five walks' label hints against
// the live consoles. The page PRINTS it once, as `labels read <date>` in the
// small right-aligned line under the walk's list (§20 errata 309-311), for the reason
// `Preset.Source()` prints the date beside every price: a vendor's menus rot
// on the vendor's schedule, and a hint whose age is unstated is a hint a
// reader has to trust blind. Re-reading the consoles once a year and moving
// this string is the maintainer's yearly task.
const walksRead = "2026-09-13"

// blockHint is the one line under every bucket block (§20 erratum 309: the
// five bucket hints and two local hints of erratum 298 collapsed to it). It
// names the failure somebody has actually had: a placeholder left in. Its
// second sentence — an empty credential booting on the bundled server's
// published keys — went with that server (§20 erratum 345): compose no longer
// falls back to any key, so an empty one is a boot error naming itself. The
// directory block carries no hint: its one placeholder explains itself.
const blockHint = "Fill every <…>."

// localTunnelNote is the folder walk's closing line on a tunnel — Cloudflare's
// or a Clippet Pass name's (§20 erratum 348 (b), which withdrew erratum 299's
// refusal on the owner's 2026-10-14 verdict: name the risk, never refuse). It is
// the desktop app's own folder-card hint (clippet-pass `LOCAL_HINT`, signed),
// with "this PC" spelled the way this page spells the machine.
const localTunnelNote = "Clips live in a folder here; the link plays from this machine through Cloudflare's tunnel. " +
	"Their terms reserve video for a bucket — at this size the worst on record is an email."

// setupStep is one step of a walk: the INSTRUCTION, in terms of the artifact
// and its properties — true for as long as the provider offers the service —
// and, under it, a DATED hint naming today's labels.
//
// The split is the fix for a real failure: Backblaze documents two live web
// consoles with two different vocabularies, and two readings of this walk each
// concluded the other was stale. A person who cannot find the named button
// still knows what they are looking for; a person who can saves a minute.
type setupStep struct {
	Text string
	Hint string
}

// setupOption is one row of the chooser and the whole of its walk.
type setupOption struct {
	// ID is the `?store=` value and the id the diagonal is compared against.
	ID string
	// Label is the radio card's name; Hint is the card's one fact, in its
	// state cell (§20 erratum 311: `10 GB free · egress free`). The hint is a
	// property of the OPTION, never a statement about this instance — whether
	// it is what you are ON is said by the card's state cell reading `on now`.
	Hint  string
	Label string
	// Head is the walk's heading: the backend's own name, printed after the
	// sequence number the template owns (`2 · Cloudflare R2`, §20 errata
	// 309-311) — and Caveat its one head hint, the only caveat the walk
	// keeps; "" for two of five.
	Head   string
	Caveat string
	Steps  []setupStep
	// Block is the total `.env` block, with a trailing newline.
	Block string
	// BlockCF is the block this option prints on an instance reached THROUGH
	// CLOUDFLARE, and "" for the four options where the exposure changes
	// nothing about it (§20 erratum 298 (f)). Exactly one walk needs it: "any
	// other S3-compatible bucket" is the only target whose endpoint may be
	// private — a NAS, a LAN address — so it is the only one where
	// `MEDIA_DELIVERY` is not a function of the target alone. It is a whole
	// SECOND BLOCK rather than a line the page tells you to edit, because a
	// block that has to be edited is the class of instruction the total-block
	// rule exists to abolish.
	BlockCF string
	// Note is the option's own closing line under the block, and NoteCF the
	// one that replaces it on a tunnel (Cloudflare's or a Pass name's). The
	// `other` walk has both; the folder has NoteCF alone — the risk, erratum
	// 348 (b); the three hosted buckets have neither.
	Note   string
	NoteCF string
	// Services is what `docker compose ps` should show afterwards — the
	// observable half of the COMPOSE_FILE assignment, and the cheapest
	// possible check that the block took effect.
	Services string
	// Backend is the STORAGE_BACKEND this option lands on, Local says the
	// bytes stay on this machine (which a Cloudflare tunnel then streams — the
	// risk its NoteCF names, erratum 348 (b)), and Bucket says the shared
	// bucket hints apply.
	Backend string
	Local   bool
	Bucket  bool
}

// composeSep is the first assignment of every block (§20 erratum 348 (e)):
// `COMPOSE_FILE=a:b` is split on `:` only where compose's default separator is
// `:`, and on Windows it is `;` — so without this line every block's file list
// is one file name nobody has. It is inert on macOS and Linux.
const composeSep = "COMPOSE_PATH_SEPARATOR=:\n"

// setupOptions is the table, in the owner's order: local first on purpose — and
// since §20 erratum 345 local is also the shipped default — then the
// recommendation, then the rest. (The bundled MinIO, the old shipped default,
// was the sixth and last row until that erratum withdrew it.)
//
// It is a function returning a fresh slice rather than a package var so
// nothing can mutate the table from a handler.
func setupOptions() []setupOption {
	return []setupOption{
		{
			ID:       config.BackendFS,
			Label:    "A folder or NAS on this machine",
			Hint:     "free · machine must be on",
			Head:     "A folder or NAS on this machine",
			Backend:  config.BackendFS,
			Local:    true,
			Services: "1 service: app",
			Steps: []setupStep{
				{Text: "Pick the host directory that will hold the clips. Default: a `clippet` folder in your Videos folder.",
					Hint: "NAS: mount the share on the host, not inside the container"},
				{Text: "It must not be inside the data or scratch directory. The app refuses to boot if it is."},
				{Text: "Make uid 10001 own it. SMB/CIFS: mount with `uid=10001,gid=10001,file_mode=0644,dir_mode=0755,nofail,_netdev`.",
					Hint: "`chown -R 10001:10001` on a plain disk"},
				{Text: "Paste the block into .env."},
				{Text: "Back up the directory. It holds a copy of the database too."},
			},
			// STORAGE_DIR_HOST is commented out (§20 erratum 348 (e)): the
			// default — a `clippet` folder in your Videos folder — works, and a
			// block that makes a desktop user fill a line the default answers is
			// a block that gets a placeholder pasted into it.
			Block: "# .env — a directory on this machine, one container, no bucket\n" +
				composeSep +
				"COMPOSE_FILE=compose.yaml:compose.fs.yaml\n" +
				"STORAGE_BACKEND=fs\n" +
				"#STORAGE_DIR_HOST=<the host directory, or your NAS mount point>\n" +
				"MEDIA_DELIVERY=proxy\n" +
				"STORAGE_PRICING=local\n" +
				"# The four S3_* lines are unused on this flavour and compose supplies them\n" +
				"# EMPTY.\n" +
				"# If your uplink is METERED the disk is free and the household bill is not:\n" +
				"# type EGRESS_PRICE_PER_GB over the preset.\n",
			NoteCF: localTunnelNote,
		},
		{
			ID:       "r2",
			Label:    "Cloudflare R2",
			Hint:     "10 GB free · egress free",
			Head:     "Cloudflare R2",
			Backend:  config.BackendS3,
			Bucket:   true,
			Services: "1 service: app",
			Steps: []setupStep{
				{Text: "Add R2 to your Cloudflare account. Free to start; it asks for a payment method.",
					Hint: "Storage & databases → R2 → Overview"},
				{Text: "Make a private bucket, storage class Standard."},
				{Text: "Make an account API token, Object Read & Write, scoped to that bucket.",
					Hint: "R2 → Account details → API Tokens → Create Account API token"},
				{Text: "Copy the access key id, the secret and the endpoint now. The secret is shown once."},
				{Text: "Paste the block into .env."},
			},
			Block: "# .env — Cloudflare R2\n" +
				composeSep +
				"COMPOSE_FILE=compose.yaml:compose.nobucket.yaml\n" +
				"STORAGE_BACKEND=s3\n" +
				"S3_ENDPOINT=<paste the endpoint the token dialog showed — whole, scheme and all>\n" +
				"S3_REGION=auto\n" +
				"S3_BUCKET=<your bucket>\n" +
				"S3_ACCESS_KEY=<access key id>\n" +
				"S3_SECRET_KEY=<secret access key>\n" +
				"S3_FORCE_PATH_STYLE=false\n" +
				"S3_KEY_PREFIX=\n" +
				"MEDIA_DELIVERY=redirect\n" +
				"STORAGE_PRICING=r2\n",
		},
		{
			ID:       "b2",
			Label:    "Backblaze B2",
			Hint:     "10 GB free · no card",
			Head:     "Backblaze B2",
			Caveat:   "Egress free up to three times what you store.",
			Backend:  config.BackendS3,
			Bucket:   true,
			Services: "1 service: app",
			Steps: []setupStep{
				{Text: "Create a Backblaze account. No card."},
				{Text: "Make a private bucket. Leave object lock off.",
					Hint: "B2 Cloud Storage → Buckets → Create a Bucket"},
				{Text: "Find the bucket's S3 endpoint and the region string inside it. You need both.",
					Hint: "on the bucket's row or details panel"},
				{Text: "Make a key for that bucket only: read and write, no prefix, no expiry.",
					Hint: "Standard console: B2 Cloud Storage → Application Keys → Add a New Application Key. Enterprise console: Access Control → Application Keys → Create App Key."},
				{Text: "Copy the key id and the application key now. The key is shown once. Do not use the master key."},
				{Text: "Add a lifecycle rule that cancels unfinished large files after a day. B2 has no default.",
					Hint: "the bucket's lifecycle settings"},
				{Text: "Paste the block into .env."},
			},
			Block: "# .env — Backblaze B2\n" +
				composeSep +
				"COMPOSE_FILE=compose.yaml:compose.nobucket.yaml\n" +
				"STORAGE_BACKEND=s3\n" +
				"S3_ENDPOINT=<paste the bucket's endpoint — whole, scheme and all>\n" +
				"S3_REGION=<the region string inside that endpoint>\n" +
				"S3_BUCKET=<your bucket>\n" +
				"S3_ACCESS_KEY=<the key id>\n" +
				"S3_SECRET_KEY=<the application key>\n" +
				"S3_FORCE_PATH_STYLE=false\n" +
				"S3_KEY_PREFIX=\n" +
				"MEDIA_DELIVERY=redirect\n" +
				"STORAGE_PRICING=b2\n",
		},
		{
			ID:       "s3",
			Label:    "Amazon S3",
			Hint:     "no free tier",
			Head:     "Amazon S3",
			Caveat:   "A free AWS account closes after six months. Do not keep a server's clips on one.",
			Backend:  config.BackendS3,
			Bucket:   true,
			Services: "1 service: app",
			Steps: []setupStep{
				{Text: "Make a bucket near your viewers. Leave “Block all public access” on; no CORS rule or bucket policy is needed."},
				{Text: "Add a lifecycle rule that deletes incomplete multipart uploads after a day.",
					Hint: "the bucket's Management tab"},
				{Text: "Make a policy for this bucket only: ListBucket on the bucket; GetObject, PutObject, DeleteObject, AbortMultipartUpload and ListMultipartUploadParts on its objects. The JSON is in DEPLOY §35."},
				{Text: "Make a user with no console access, attach the policy, create an access key. The secret is shown once.",
					Hint: "IAM access keys, the “application running outside AWS” case"},
				{Text: "The endpoint is built from your region. Region codes are in DEPLOY §35."},
				{Text: "Paste the block into .env."},
			},
			Block: "# .env — Amazon S3\n" +
				composeSep +
				"COMPOSE_FILE=compose.yaml:compose.nobucket.yaml\n" +
				"STORAGE_BACKEND=s3\n" +
				"S3_ENDPOINT=<scheme>://s3.<your region>.amazonaws.com\n" +
				"S3_REGION=<your region>\n" +
				"S3_BUCKET=<your bucket>\n" +
				"S3_ACCESS_KEY=<access key id>\n" +
				"S3_SECRET_KEY=<secret access key>\n" +
				"S3_FORCE_PATH_STYLE=false\n" +
				"S3_KEY_PREFIX=\n" +
				"MEDIA_DELIVERY=redirect\n" +
				"# The s3 preset models NO free storage on purpose. What a new account gets is a\n" +
				"# credit, not an allowance, and pricing a credit nobody is still on would\n" +
				"# understate every later bill.\n" +
				"STORAGE_PRICING=s3\n",
		},
		{
			ID:       "other",
			Label:    "Another S3-compatible bucket",
			Hint:     "your prices",
			Head:     "Another S3-compatible bucket",
			Caveat:   "MinIO on a NAS, Wasabi, Hetzner, Garage, Ceph.",
			Backend:  config.BackendS3,
			Bucket:   true,
			Services: "1 service: app",
			Steps: []setupStep{
				{Text: "Make a private bucket."},
				{Text: "Make a key that can read, write, delete and abort multipart uploads in that bucket only."},
				{Text: "Find the endpoint, with its scheme. A LAN address is fine."},
				{Text: "Path style for self-hosted servers, virtual host for hosted providers. A DNS failure with the bucket name in front of the host means it is the wrong way round."},
				{Text: "This block proxies the video through this instance, which a private endpoint needs. The note below says when to switch."},
				{Text: "If the provider offers it, abort incomplete multipart uploads after a day."},
				{Text: "Type your provider's two prices, or set STORAGE_PRICING to the nearest preset."},
				{Text: "Paste the block into .env."},
			},
			Block: "# .env — any S3-compatible bucket\n" +
				composeSep +
				"COMPOSE_FILE=compose.yaml:compose.nobucket.yaml\n" +
				"STORAGE_BACKEND=s3\n" +
				"S3_ENDPOINT=<scheme>://<host>[:<port>]\n" +
				"S3_REGION=<whatever the provider calls it; MinIO ignores it>\n" +
				"S3_BUCKET=<your bucket>\n" +
				"S3_ACCESS_KEY=<key id>\n" +
				"S3_SECRET_KEY=<secret>\n" +
				"S3_FORCE_PATH_STYLE=<true for a self-hosted server, false for a hosted one>\n" +
				"S3_KEY_PREFIX=<optional, if you are sharing a bucket>\n" +
				"MEDIA_DELIVERY=proxy\n" +
				"STORAGE_PRICE_PER_GB_MONTH=<what keeping 1 GB for a month costs you>\n" +
				"EGRESS_PRICE_PER_GB=<what sending 1 GB costs you; omit if it is free>\n",
			BlockCF: "# .env — any S3-compatible bucket, on a Cloudflare tunnel\n" +
				composeSep +
				"COMPOSE_FILE=compose.yaml:compose.nobucket.yaml\n" +
				"STORAGE_BACKEND=s3\n" +
				"S3_ENDPOINT=<scheme>://<host>[:<port>]\n" +
				"S3_REGION=<whatever the provider calls it; MinIO ignores it>\n" +
				"S3_BUCKET=<your bucket>\n" +
				"S3_ACCESS_KEY=<key id>\n" +
				"S3_SECRET_KEY=<secret>\n" +
				"S3_FORCE_PATH_STYLE=<true for a self-hosted server, false for a hosted one>\n" +
				"S3_KEY_PREFIX=<optional, if you are sharing a bucket>\n" +
				"MEDIA_DELIVERY=redirect\n" +
				"STORAGE_PRICE_PER_GB_MONTH=<what keeping 1 GB for a month costs you>\n" +
				"EGRESS_PRICE_PER_GB=<what sending 1 GB costs you; omit if it is free>\n",
			Note:   "Set MEDIA_DELIVERY=redirect only if the endpoint is public https (DEPLOY.md §33).",
			NoteCF: "Behind Cloudflare the endpoint must be public https: a LAN address or a NAS will not work.",
		},
	}
}

// setupOptionByID finds one option. ok is false for an unknown value, which
// the handler renders as the chooser and no panel — never a 500 and never a
// 404, because a mistyped query is not a missing page.
func setupOptionByID(id string) (setupOption, bool) {
	for _, o := range setupOptions() {
		if o.ID == id {
			return o, true
		}
	}
	return setupOption{}, false
}

// ---- what is true now ----------------------------------------------------

// The two shapes this instance's CURRENT store can be in, from the page's
// point of view. They are two and not five because two is all the process can
// honestly tell apart: `fs` is recognisable, and R2, B2, S3 and a box on the
// LAN are one answer — the page prints no endpoint host (§20 erratum 297), and
// it could not distinguish them if it did. (A third, the bundled MinIO, was
// recognised by its endpoint until §20 erratum 345 withdrew that server.)
const (
	currentFS     = "fs"
	currentBucket = "bucket"
)

// currentStore names which of the two this instance is on.
func currentStore(cfg config.Config) string {
	if cfg.StorageBackend == config.BackendFS {
		return currentFS
	}
	return currentBucket
}

// backendWord is the first panel's `backend` cell.
func backendWord(current string) string {
	if current == currentFS {
		return "a directory (fs)"
	}
	return "a bucket (s3)"
}

// whereWord is the first panel's `where` cell. On a bucket it is deliberately
// thin: the host, the bucket and the prefix are NOT printed (§20 erratum 297).
// On a directory the container path is — it is neither a credential nor a name
// anybody could reach, and it is the one thing an operator needs to recognise
// a bind mount they got wrong.
func whereWord(cfg config.Config, current string) string {
	if current == currentFS {
		return cfg.StorageDir + " · in the container; the host side is your bind mount"
	}
	return "a bucket · keys set, never shown"
}

// ---- the STOP callout ----------------------------------------------------

// setupStop is the state-gated STOP as the walk panel's first thing, whenever
// this instance holds clips and the target is not what it is on (§20 errata
// 297, 309-311): a red-hairline callout whose bold lead IS the fact — the
// count, and that it stays put — then what does not happen on its own, and
// what comes first. Three strings and not one, because the template sets the
// verb in <code> between the second and the third: the template does no
// comparison, so the split is decided here.
//
// Three things it deliberately does not do. It prints no command: DEPLOY
// §32.3's correct form carries a bind whose omission silently writes tens of
// gigabytes into the container's writable layer, and a page that prints a
// data-moving command must print the documented one or none. It offers no
// button: a browser control that moves 45 GiB is a different product. And it
// does not BLOCK — the operator may have moved the clips already, or may not
// want them. It is a STOP, not a gate.
type setupStop struct {
	// Lead is the bold sentence: `5 clips stay where they are.`
	Lead string
	// Before runs up to the verb (`Switching does not move them. Move them
	// first: `); After is what follows it (` (DEPLOY.md §32.3).`, and between
	// two buckets the sentence that says the page cannot tell).
	Before, After string
}

// stopVerb is the one verb the STOP names, set in <code> by the template so an
// operator knows what to go and read — and never a runnable form of it.
const stopVerb = "admin storage migrate"

// stopFor decides the STOP for one pick. nil when it does not apply: no clips,
// or the diagonal.
//
// Between two buckets the page cannot know whether the target is the bucket it
// is already on, so the sentence says so in words rather than guessing. That
// is the honest form of a warning whose precondition is invisible.
func stopFor(current string, target setupOption, clips int) *setupStop {
	if clips <= 0 || diagonal(current, target) {
		return nil
	}
	lead, them := strconv.Itoa(clips)+" clips stay where they are.", "them"
	if clips == 1 {
		lead, them = "1 clip stays where it is.", "it"
	}
	st := &setupStop{Lead: lead, Before: "Switching does not move " + them + ". ", After: " (DEPLOY.md §32.3)."}
	if current == currentBucket && target.Bucket {
		st.Before += "If the new bucket is a different one, move " + them + " first: "
		st.After += " This page cannot tell whether it is."
		return st
	}
	st.Before += "Move " + them + " first: "
	return st
}

// Text is the callout as one plain sentence — what a test reads, and what the
// no-URL tripwire is run over.
func (st *setupStop) Text() string {
	if st == nil {
		return ""
	}
	return st.Lead + " " + st.Before + stopVerb + st.After
}

// diagonal is "this is what you are on now": an exact match of the derived
// current store against the target. An instance on an external bucket has no
// exact match among the four bucket options, which is what the conditional
// STOP above is for. The card of the diagonal option reads `on now`.
func diagonal(current string, target setupOption) bool {
	return current == currentFS && target.ID == config.BackendFS
}

// ---- how this instance is reached ---------------------------------------

// The three exposures the page classifies (§20 errata 299, 303). "Classify the
// thing first" is the product rule, and this is the classification the
// answer actually turns on.
//
// A Clippet Pass name is its own answer and not a fourth spelling of the
// Cloudflare one, because it decides one thing more: the block gains the three
// lines a name needs. Everything the Cloudflare answer decides, it decides
// identically — a Pass name IS a Cloudflare tunnel, with the Pass operator's account at
// the other end of it instead of yours.
const (
	exposureCloudflare = "cloudflare"
	exposurePass       = "pass"
	exposureOther      = "other"
)

// exposureChoices is the `Reached` control: a `.seg` of three radios (a closed
// set of three is a seg, never a <select> — web/STYLE.md), whose label reads
// with the row's own: "Reached directly", "Reached via Cloudflare", "Reached
// via a Clippet Pass". No hint: what it changes is said in the walk it
// decides (§20 errata 309, 348 (b)).
func exposureChoices() []setupChoice {
	return []setupChoice{
		{Value: exposureOther, Label: "directly"},
		{Value: exposureCloudflare, Label: "via Cloudflare"},
		{Value: exposurePass, Label: "via a Clippet Pass"},
	}
}

// setupChoice is one radio card of the storage backend chooser or one chip of
// the Reached seg, pre-rendered: the template does no comparison of its own.
// State is the card's state cell — the option's one fact, or `on now` on the
// diagonal (§20 errata 311, 348 (b): no pick is refused any more); the seg's
// chips carry none.
type setupChoice struct {
	Value, Label, Hint string
	Selected           bool
	State              string
}

// passComposeFile is the compose file a Clippet Pass name adds, appended to
// whichever COMPOSE_FILE the chosen store already needs.
const passComposeFile = "compose.pass.yaml"

// passSessionCookie is the cookie name a name on a shared parent domain must
// use. `__Host-` pins the cookie to this exact host with no `Domain=`, which is
// what stops one name on the zone from being handed a cookie for another; the
// loader refuses the prefix on a plain-http base, so it belongs in the block
// from the first boot rather than in an edit afterwards.
const passSessionCookie = "__Host-clippet_session"

// passScheme is the `https` of the Pass block's PUBLIC_BASE_URL, spelled apart
// from its separator on purpose.
//
// Law 1 at the top of this file is enforced by a line-based grep over every
// .go and .html under internal/, cmd/ and web/ (`scripts/check.sh`, §15): a
// literal `http(s)://` fails it wherever it appears, comment included. The
// stage's own message names the two ways out — say it in words, or split the
// scheme — and here the second is the honest one, because unlike every other
// endpoint on this page this scheme is not the operator's to choose: a Pass
// name is served over TLS by the Pass edge and by nothing else, so a
// `<scheme>://` placeholder would be asking a question with one answer. The
// rendered line is the whole literal, and setup_test.go's golden — which the
// grep exempts, being a test — pins it as bytes.
const passScheme = "https"

// passBlockFor turns the block a target prints on Cloudflare into the block it
// prints on a Clippet Pass name: the same store, the same MEDIA_DELIVERY, plus
// the three lines a name needs and `compose.pass.yaml` on the file list.
//
// The COMPOSE_FILE line is EXTENDED rather than added, which is the whole
// reason this is code and not a seventh table row. Law 2 says a block assigns
// every variable that decides the outcome and law 2's test counts the
// assignments: two COMPOSE_FILE lines in one paste is a file list somebody has
// to merge by hand, and the second one silently wins. Deriving the Pass block
// from the target's own means a new store option is on a Pass name the day it
// is added, with no second place to forget.
//
// It OPENS with the release (§20 erratum 348 (e)): a Pass box built from an
// arbitrary checkout is one the plane cannot match to a release, and without
// CLIPPET_COMMIT the portal calls it behind for ever — so the first thing the
// block says, under its name, is which tree to build and the line that names it.
func passBlockFor(o setupOption) string {
	base, _ := setupBlockFor(o, exposureCloudflare)
	var b strings.Builder
	for i, line := range strings.SplitAfter(base, "\n") {
		// The block's first line NAMES it, and a pasted block has to be
		// recognisable in a diff a year later, so the Pass says so there: the
		// store's own head, with "on a Cloudflare tunnel" replaced rather than
		// stacked, because a Pass name IS that tunnel.
		if i == 0 {
			b.WriteString(strings.TrimSuffix(strings.TrimRight(line, "\n"), ", on a Cloudflare tunnel") +
				", on a Clippet Pass name\n")
			b.WriteString(passRelease)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "COMPOSE_FILE="); ok {
			b.WriteString("COMPOSE_FILE=" + strings.TrimRight(rest, "\n") + ":" + passComposeFile + "\n")
			continue
		}
		b.WriteString(line)
	}
	b.WriteString("# ...and the name itself. The first two are this app's own and have to be\n")
	b.WriteString("# right from the FIRST boot on the name: every card tag and every share\n")
	b.WriteString("# link is built from the base URL, and the cookie prefix is refused on a\n")
	b.WriteString("# plain-http base. The third belongs to the Pass container and reaches\n")
	b.WriteString("# this app never.\n")
	b.WriteString("PUBLIC_BASE_URL=" + passScheme + "://<name>.clippet.gg\n")
	b.WriteString("SESSION_COOKIE_NAME=" + passSessionCookie + "\n")
	b.WriteString("CLIPPET_PASS_TOKEN=<paste the token from your Clippet portal>\n")
	return b.String()
}

// passRelease is the Pass block's opening, under its name (erratum 348 (e)):
// the release to build and the line that names its commit (DEPLOY.md §36.2).
const passRelease = "# First: check out the latest vYYYY.MM.DD tag and set CLIPPET_COMMIT to its\n" +
	"# commit (DEPLOY.md §36.2).\n" +
	"CLIPPET_COMMIT=<the 40-hex commit of that tag: git rev-parse vYYYY.MM.DD^{commit}>\n"

// passNote is the Pass block's own closing line, under the target's.
const passNote = "Set CLIPPET_COMMIT beside CLIPPET_VERSION (DEPLOY.md §16), or the portal calls this Clippet behind."

// tunnelProxies is the line every block printed for a Cloudflare tunnel ends
// with, and so every Clippet Pass block too, which is derived from it (DEPLOY.md
// §4). Behind a tunnel every viewer arrives from the tunnel container's one
// address on the compose network; without the line the per-IP limits and the
// view dedupe key all of them together as one client. The prefix is the one
// DEPLOY §4 names, and the app port stays on loopback, which is what makes
// trusting it safe.
const tunnelProxies = "TRUSTED_PROXIES=172.16.0.0/12\n"

// setupBlockFor is the block this option prints for this exposure, and the
// closing lines that go under it (§20 errata 298 (f), 303).
//
// Four of the five options answer the same thing on the first two exposures. The
// fifth — "any other S3-compatible bucket" — is the only target whose endpoint
// may be one a viewer's browser cannot reach, so it is the only one whose
// MEDIA_DELIVERY is not decided by the target alone: `proxy` by default,
// because that is what a NAS or a LAN address needs and what the walk's own
// caveat names first, and `redirect` behind Cloudflare, because there the
// bytes should not come out of this box at all (erratum 299) — which also makes a
// private endpoint no longer usable, and the note beside it says so. Every
// block on a tunnel ends with tunnelProxies (DEPLOY.md §4), the one line the
// tunnel itself decides.
//
// A Clippet Pass name is the Cloudflare answer plus three lines (erratum 303),
// derived by passBlockFor above. A LOCAL target walks on both like any other
// (§20 erratum 348 (b), which withdrew erratum 299's refusal): its block is the
// folder block plus tunnelProxies, `proxy` still — `redirect` on a directory is
// a boot error — and its closing line is the risk, localTunnelNote.
func setupBlockFor(o setupOption, exposure string) (block string, notes []string) {
	if exposure == exposurePass {
		_, cfNotes := setupBlockFor(o, exposureCloudflare)
		return passBlockFor(o), append(cfNotes, passNote)
	}
	if exposure == exposureCloudflare {
		if o.BlockCF != "" {
			return o.BlockCF + tunnelProxies, nonEmpty(o.NoteCF)
		}
		if o.NoteCF != "" {
			return o.Block + tunnelProxies, nonEmpty(o.NoteCF)
		}
		return o.Block + tunnelProxies, nonEmpty(o.Note)
	}
	return o.Block, nonEmpty(o.Note)
}

// nonEmpty is the one-or-none closing line as a list: four of five options have
// no line of their own, and an empty string is not a line.
func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
