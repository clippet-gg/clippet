package main

// admin_collections.go — `clippet admin collections ls|rm` (§20 erratum 275).
//
// The one leftover the E2E's own sweep could not clear. `users prune` skips an
// account that still owns a COLLECTION (§20 erratum 86: a user who owns
// anything is never a candidate), a collection is created and deleted only
// through its owner's SESSION — which an interrupted run took with it — and no
// admin verb could remove one. So a run that died between "make a collection"
// and "delete it" left an account on the stack that every later run NOTEd and
// no run could clear (§20 erratum 272(c), lane litter).
//
// `rm` REMOVES THE COLLECTION ROW AND NOTHING ELSE. It is db.DeleteCollection,
// the same statement the owner's own DELETE route calls: the membership rows go
// with it through the FK cascade and not one clip is touched, because a
// collection is a list of links and deleting the list deletes links. That is the
// whole reason this verb is safe to add to a CLI whose destructive commands all
// need an explicit selector (DEPLOY §24): there is no data behind a collection
// to lose.
//
// Both halves follow the house rules for a destructive verb: the target is
// PRINTED in full before anything is removed (the destructive-action rule: the
// exact target is printed first and an explicit yes is needed), an unknown id is a
// clear refusal rather than a silent success, and `--owner` is an optional
// second selector that must AGREE with the row — so an operator who names the
// account they mean cannot remove somebody else's collection by mistyping an id.

import (
	"errors"
	"fmt"
	"strings"

	"clippet/internal/db"
)

func (a *admin) collectionsUsage() {
	fmt.Fprintln(a.env.stderr, "usage: clippet admin collections ls [--owner <username>]")
	fmt.Fprintln(a.env.stderr, "       clippet admin collections rm <collection-id> [--owner <username>]")
	fmt.Fprintln(a.env.stderr, "  rm removes the COLLECTION ROW only: its clips are never touched.")
}

// collections is the inventory (`ls`) and its one destructive verb (`rm`).
// There is no bare `collections` form: `ls` is one more word and reads as what
// it does, and a verb-less default on a command that also removes things is
// exactly the shape this CLI avoids.
func (a *admin) collections(args []string) int {
	if len(args) == 0 {
		a.collectionsUsage()
		return 2
	}
	switch args[0] {
	case "ls":
		return a.collectionsLs(args[1:])
	case "rm":
		return a.collectionsRm(args[1:])
	}
	a.collectionsUsage()
	return 2
}

// collectionsLs prints the inventory, newest first, in the same grammar
// `admin ls` uses for clips: a header, one row per collection, and a count line
// that names the filter — so a short listing can never be mistaken for an empty
// instance. The TITLE is last and is quoted (%q), because it is the one field a
// person typed: quoting it keeps a title containing spaces, a newline or a tab
// inside its own row, which is what makes the listing safe to parse by column.
func (a *admin) collectionsLs(args []string) int {
	fs := a.flags("collections ls")
	owner := fs.String("owner", "", "only collections owned by this username")
	extra, err := parseWithID(fs, args)
	if err != nil || extra != "" {
		a.collectionsUsage()
		return 2
	}
	ownerID := ""
	if *owner != "" {
		u, ok := a.resolveUser(*owner)
		if !ok {
			return 1
		}
		ownerID = u.ID
	}
	rows, err := a.db.ListCollectionSummaries(a.ctx, ownerID)
	if err != nil {
		return a.fail("collections ls", err)
	}
	if len(rows) == 0 {
		fmt.Fprintf(a.env.stdout, "no collections%s\n", collectionsFilterSuffix(*owner))
		return 0
	}
	fmt.Fprintf(a.env.stdout, "%-12s %-20s %-6s %-20s %s\n", "ID", "OWNER", "CLIPS", "CREATED", "TITLE")
	for _, c := range rows {
		fmt.Fprintf(a.env.stdout, "%-12s %-20s %-6d %-20s %q\n", c.ID, c.OwnerName, c.ClipCount, rfc(c.CreatedAt), c.Title)
	}
	fmt.Fprintf(a.env.stdout, "%d collections%s\n", len(rows), collectionsFilterSuffix(*owner))
	return 0
}

// collectionsFilterSuffix names the filter on the count line (lsFilterSuffix's
// rule, for the one filter this listing has).
func collectionsFilterSuffix(owner string) string {
	if owner == "" {
		return ""
	}
	return " (owner=" + owner + ")"
}

// collectionsRm removes ONE collection row. What it prints first is the target:
// id, owner, clip count and title, in the same words `ls` uses.
func (a *admin) collectionsRm(args []string) int {
	fs := a.flags("collections rm")
	owner := fs.String("owner", "", "refuse unless the collection belongs to this username")
	id, err := parseWithID(fs, args)
	if err != nil || strings.TrimSpace(id) == "" {
		a.collectionsUsage()
		return 2
	}
	c, err := a.db.GetCollection(a.ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		// The same sentence and the same exit code an unknown clip id gets, so a
		// script driving this verb has one shape to read (DEPLOY §24).
		return a.notFound(id)
	case err != nil:
		return a.fail("collections rm", err)
	}
	u, err := a.db.GetUserByID(a.ctx, c.OwnerID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		// A collection whose owner row is gone cannot exist — `collections.owner_id`
		// CASCADEs — so this is a broken database rather than a case to handle.
		return a.fail("collections rm", fmt.Errorf("collection %s names owner %s, which has no row", id, c.OwnerID))
	case err != nil:
		return a.fail("collections rm", err)
	}
	if *owner != "" && !strings.EqualFold(*owner, u.Username) {
		fmt.Fprintf(a.env.stdout, "%s: owned by %s, not %s\n", id, u.Username, *owner)
		return 1
	}
	// THE TARGET, PRINTED BEFORE ANYTHING IS REMOVED (the destructive-action
	// rule: the exact target is printed first and an explicit yes is needed).
	fmt.Fprintf(a.env.stdout, "collection: %s  owner=%s  clips=%d  title=%q\n", c.ID, u.Username, c.ClipCount, c.Title)
	if err := a.db.DeleteCollection(a.ctx, c.ID, c.OwnerID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// It went between the read and the delete. Nothing was removed here.
			return a.notFound(id)
		}
		return a.fail("collections rm", err)
	}
	fmt.Fprintf(a.env.stdout, "removed collection %s (%d clip(s) untouched)\n", c.ID, c.ClipCount)
	return 0
}
