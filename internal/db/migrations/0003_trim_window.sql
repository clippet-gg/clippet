-- 0003_trim_window.sql — Phase 4 #5, the trim window contract.
--
-- A trim window is expressed on the ORIGINAL's timeline, always. The original
-- is retained for the life of the clip (§17 "non-destructive editing"), so the
-- coordinate system an owner cuts in must be the one thing that never moves —
-- and before these columns it was the RENDITION's, which shrank with every cut
-- until a clip trimmed once could never be re-extended.
--
-- Three nullable columns and nothing else: no index (they are only ever read
-- through a clip already in hand), no CHECK, no default. ALTER TABLE ... ADD
-- COLUMN is O(1) in SQLite and NULL is exactly what "we have not measured the
-- original yet" and "this clip is the whole original" mean, so an installation
-- running on 0001/0002 upgrades by applying this file and nothing more.
ALTER TABLE clips ADD COLUMN source_duration_ms INTEGER;  -- the ORIGINAL's container duration; NULL = not measured yet
ALTER TABLE clips ADD COLUMN trim_start_ms      INTEGER;  -- the applied window on that timeline; NULL/NULL = untrimmed
ALTER TABLE clips ADD COLUMN trim_end_ms        INTEGER;

-- Backfill, conservatively. A ready clip that has NEVER completed a trim was
-- encoded from the whole original, so its measured duration_ms IS the
-- original's length to within the encoder's rounding — good enough to bound a
-- form, and overwritten with the real probe of the original the first time the
-- clip is trimmed (SwapRendition). A clip that HAS completed a trim is exactly
-- the ambiguous case (its duration_ms describes a cut, not the upload), so it
-- keeps NULL: the API is permissive while the value is unknown, and the
-- worker's per-stream coverage check is what rejects a window past the source.
UPDATE clips SET source_duration_ms = duration_ms
WHERE state = 'ready'
  AND duration_ms IS NOT NULL
  AND id NOT IN (SELECT clip_id FROM jobs WHERE kind = 'trim' AND state = 'done');
