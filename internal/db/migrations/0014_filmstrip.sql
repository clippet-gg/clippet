-- 0014_filmstrip.sql — the trim scrubber's thumbnail filmstrip (ROADMAP #6's
-- open half; §20 erratum 220).
--
-- Seven additive columns on `clips` and nothing else: no index (they are only
-- ever read through a clip already in hand), no CHECK, no backfill. ALTER
-- TABLE ... ADD COLUMN is O(1) in SQLite and NULL is exactly what "this clip
-- has no filmstrip yet" means, so an installation running on 0001-0013
-- upgrades by applying this file and nothing more.
--
-- The filmstrip is a derived artefact of the CURRENT rendition, like the
-- preview GIF and the social rendition: one JPEG sprite sheet of tiles laid
-- out in a single row, made on demand, invalidated by a trim or a replacement
-- (both NULL these columns and delete the object, exactly as they already do
-- for gif_*), and re-made the next time somebody opens the trim control. That
-- is why there is no "generated_at" and no attempt counter here — the job
-- rows carry both, and a filmstrip that is missing is simply one nobody has
-- asked for since the last cut.
--
-- The three key columns are the same triple every other object gets
-- (key/size/etag), so `admin orphans`, the clip delete and the supersede lists
-- need no new vocabulary. The four measurements are what the CLIENT needs to
-- turn a moment into a tile: tiles across, one tile's width and height, and
-- the interval each tile covers in milliseconds. They are MEASURED from the
-- produced JPEG rather than planned, for poster_width/poster_height's reason
-- (§20 erratum 92): what the file holds is the only honest answer, and a
-- scrubber that positioned tiles from a plan the encoder did not follow would
-- show the wrong frame under the handle.
ALTER TABLE clips ADD COLUMN filmstrip_key         TEXT;     -- NULL = no filmstrip for the current rendition
ALTER TABLE clips ADD COLUMN filmstrip_size        INTEGER;
ALTER TABLE clips ADD COLUMN filmstrip_etag        TEXT;
ALTER TABLE clips ADD COLUMN filmstrip_tiles       INTEGER;  -- tiles across (the sheet is always ONE row)
ALTER TABLE clips ADD COLUMN filmstrip_tile_w      INTEGER;  -- MEASURED from the JPEG, never assumed
ALTER TABLE clips ADD COLUMN filmstrip_tile_h      INTEGER;
ALTER TABLE clips ADD COLUMN filmstrip_interval_ms INTEGER;  -- what one tile covers, on the RENDITION's timeline
