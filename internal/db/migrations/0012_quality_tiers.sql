-- 0012_quality_tiers.sql — Original / Full / Light (DESIGN.md §17 #10, §20 errata 200–211).
--
-- Twelve additive columns on `clips` and a backfill of four of them. No index
-- (every one of these is read through a clip already in hand), no CHECK, no new
-- table: ALTER TABLE ... ADD COLUMN is O(1) in SQLite, so an installation
-- running on 0001–0011 upgrades by applying this file and nothing more.
--
-- WHAT IS *NOT* HERE, and why. There is no `full_key`/`full_size`/`full_etag`
-- trio, although the Full tier is an object like any other. The Full tier's
-- object IS `rendition_key`: the row's rendition columns have always meant "the
-- main playable web file", and after these tiers that file is either the Full
-- re-encode or the `-c copy` remux of a playable original. Giving the Full its
-- own key columns would have created a second name for one object — and
-- `db.KeysOf`, `admin ls --stat`, `admin orphans`, the sweeper, the trim's
-- supersede list and StorageUsed would each have had to learn it. Every one of
-- them already covers `rendition_key`, so every one of them already covers the
-- Full tier, which is the whole reason this migration is short (§20 erratum 201).
--
-- For the same reason there are no `light_*` key columns: the Light tier is the
-- file that shipped as the "social rendition" in 0007, and it keeps its
-- `social_*` columns and its `renditions/social-{r8}.mp4` key. The rename is in
-- the API, the UI and the documentation, where a person reads it; renaming
-- columns would have rewritten every row for a word (§20 erratum 203).
--
-- `original_playable` is a FACT ABOUT THE UPLOAD as this server probed it, and
-- it is deliberately backfilled to 0 rather than guessed: no clip that exists
-- was ever probed for the property, and telling a viewer an old upload is
-- streamable when nobody checked is worse than telling them nothing. A retry or
-- a replacement re-probes and fills it in.
ALTER TABLE clips ADD COLUMN original_playable INTEGER NOT NULL DEFAULT 0;   -- the uploaded bytes are web-playable as they stand
ALTER TABLE clips ADD COLUMN original_remuxed  INTEGER NOT NULL DEFAULT 0;   -- ...after a `-c copy` container rewrite, which is `rendition_key`
ALTER TABLE clips ADD COLUMN original_width    INTEGER;                      -- the UPLOAD's own display dims, never the rendition's
ALTER TABLE clips ADD COLUMN original_height   INTEGER;
ALTER TABLE clips ADD COLUMN original_fps_num  INTEGER;                      -- the upload's average rate, exact (30000/1001 stays that)
ALTER TABLE clips ADD COLUMN original_fps_den  INTEGER;
ALTER TABLE clips ADD COLUMN original_codec    TEXT NOT NULL DEFAULT '';     -- "h264", "hevc", "av1", ... as ffprobe named it

-- The two tier STATES: '' (unknown — a row nothing has re-probed), 'skipped'
-- (not needed, which is a finished answer), 'encoding', 'ready', 'failed'.
-- They are columns and not a join because ClipDTO is rendered from a clip row
-- alone on six routes, and a per-tier job lookup on each of them would be six
-- new queries for two words.
ALTER TABLE clips ADD COLUMN full_state  TEXT NOT NULL DEFAULT '';
ALTER TABLE clips ADD COLUMN full_codec  TEXT NOT NULL DEFAULT '';           -- which encoder produced the Full ("h264" | "av1")
ALTER TABLE clips ADD COLUMN light_state TEXT NOT NULL DEFAULT '';

-- The Light copy's measured frame rate, beside the dims 0007 added and for the
-- same reason: the quality selector states "Light 720p30" from what was
-- MEASURED, and a label that states a number nobody measured is the bug erratum
-- 123 closed for the card's dimensions.
ALTER TABLE clips ADD COLUMN social_fps_num INTEGER;
ALTER TABLE clips ADD COLUMN social_fps_den INTEGER;

-- The backfill, and its one restriction: `state = 'ready'`. For a ready row
-- written before this migration the rendition IS the Full tier — it was encoded
-- by libx264 from a source nobody asked about playability — and a social file,
-- if there is one, IS the Light tier. Both are statements of fact about rows
-- that already exist. A queued, processing or failed clip is left at '' because
-- its tiers have not been decided yet, and the run that decides them writes
-- these columns itself.
UPDATE clips SET
  full_state  = 'ready',
  full_codec  = 'h264',
  light_state = CASE WHEN social_key IS NOT NULL AND social_key <> '' THEN 'ready' ELSE 'skipped' END
WHERE state = 'ready';
