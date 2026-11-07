-- 0002_gif.sql — Phase 4 #6 (GIF export).
--
-- Five nullable columns on clips and nothing else: no index (the GIF is only
-- ever reached through a clip that is already in hand), no CHECK, no default,
-- no rewrite of an existing row. ALTER TABLE ... ADD COLUMN is O(1) in SQLite
-- and every column is NULL for every clip that already exists, which is
-- exactly what "this clip has no GIF yet" means — so an installation that has
-- been running on 0001 upgrades by applying this file and nothing more.
--
-- The GIF is a DERIVED preview of the rendition, like the poster: it carries
-- its own key, size and hex-sha256 ETag (the media routes' ETag comes from
-- the row, never from the bucket) and its own measured dimensions, and it is
-- NULLed again whenever the rendition it was cut from is replaced (a trim).
ALTER TABLE clips ADD COLUMN gif_key    TEXT;      -- renditions/preview-{r8}.gif; NULL = not generated
ALTER TABLE clips ADD COLUMN gif_size   INTEGER;
ALTER TABLE clips ADD COLUMN gif_etag   TEXT;      -- hex sha256 computed while uploading (PutResult.SHA256)
ALTER TABLE clips ADD COLUMN gif_width  INTEGER;   -- MEASURED from the produced GIF, not planned
ALTER TABLE clips ADD COLUMN gif_height INTEGER;
