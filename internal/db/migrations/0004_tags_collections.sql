-- 0004_tags_collections.sql — Phase 4 #9 (tags + collections).
--
-- Three tables, no column on `clips`. Tags and collections are METADATA about
-- clips, not facts about the media, and both are many-to-many: a clip carries
-- several tags and sits in several collections, so a column would have to hold
-- a list and every query over it would be a LIKE.
--
-- Nothing here is a social feature (medal doc, anti-features). There are no
-- counters, no authorship on a tag, no cross-user surface: a tag is a word the
-- owner writes on their own clip, and a collection is an ordered list of links
-- that only its owner can change.

-- clip_tags — the tag set of one clip. The PRIMARY KEY is the whole row, so a
-- duplicate insert is a constraint failure rather than a second row and the
-- table can never hold the same tag twice for one clip. Tags are stored in
-- their NORMALISED form only (trimmed, ASCII-lowercased, matching
-- ^[a-z0-9][a-z0-9-]{0,31}$ — db.CleanTag): the wire format and the storage
-- format are the same string, so `WHERE tag = ?` is an exact match and needs
-- no COLLATE and no function index.
--
-- ON DELETE CASCADE is the whole delete story: a clip that goes (owner DELETE
-- or a TTL sweep, §12) takes its tags with it, and the sweeper needs no new
-- step.
CREATE TABLE clip_tags (
  clip_id TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
  tag     TEXT NOT NULL,
  PRIMARY KEY (clip_id, tag)
);
-- The PK covers "the tags of this clip"; this index covers the other
-- direction, "which clips carry this tag", which is what GET /api/clips?tag=
-- and the library filter run. It is only reachable from an UNCORRELATED
-- predicate: db.tagFilterSQL is `clips.id IN (SELECT clip_id FROM clip_tags
-- WHERE tag = ?)`, which the planner runs as one covering-index range scan
-- here (LIST SUBQUERY) and then looks the ids up in the clips PK. A
-- correlated `EXISTS (… t.clip_id = clips.id AND t.tag = ?)` would instead be
-- driven from the PRIMARY KEY once per candidate row and leave this index
-- dead (§20 erratum 84).
CREATE INDEX clip_tags_tag ON clip_tags(tag, clip_id);

-- collections — an ordered list of clip links, owned by one user.
--
-- There is deliberately NO `unlisted` column (§20 erratum 82): collections are
-- never listed to anyone but their owner — there is no shared collection
-- library and no browse page — so a flag distinguishing "listed" from
-- "unlisted" would have nothing to distinguish. The id is the access control,
-- exactly as it is for a clip: 12 crypto-random base62 characters from
-- ids.New() (§7), unguessable, and the link is the auth.
CREATE TABLE collections (
  id         TEXT PRIMARY KEY,                                  -- public slug (§7), same generator as clips
  owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title      TEXT NOT NULL,                                     -- cleanText, ≤200 runes, like a clip title
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX collections_owner ON collections(owner_id, created_at DESC, id);

-- collection_clips — the ordered membership. `position` is assigned by the one
-- writer (SetCollectionClips replaces the whole list in a single transaction),
-- so it is dense and gap-free by construction and nothing has to renumber.
--
-- Both foreign keys cascade, which is what makes membership self-maintaining:
-- deleting a clip removes it from every collection it was in, and deleting a
-- collection removes its rows without touching a single clip.
CREATE TABLE collection_clips (
  collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
  clip_id       TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,
  PRIMARY KEY (collection_id, clip_id)
);
CREATE INDEX collection_clips_order ON collection_clips(collection_id, position);
