-- 0018_drop_seen.sql — what the drop folder has already decided about
-- (ROADMAP #42 (b), DESIGN.md §20 erratum 296).
--
-- The drop folder turns a video left at the ROOT of STORAGE_DIR into a clip.
-- Everything else about that is in memory and can be recomputed: which files
-- are there, how big they were one tick ago, whether the probe likes them. One
-- fact cannot be, and it is the one a restart would get wrong — **this file has
-- already been ingested**. The window is real and narrow: the watcher moves a
-- file into its staging directory, ingests it, records the clip, and only then
-- removes the staged copy. A process killed in the middle leaves a staged file
-- that the next boot puts back at the root, and without this table that file
-- would become a SECOND clip of the same video, every restart, for ever.
--
-- The key is what a filesystem can tell us about a file without reading it:
-- its NAME, its SIZE and its MTIME. Not a hash — hashing every candidate on
-- every tick would read the whole folder from a NAS share every few seconds to
-- answer a question that only matters at a restart. Three facts are enough for
-- the thing this prevents (the same file, put back by our own recovery pass,
-- byte-for-byte) and are deliberately not enough to recognise a DIFFERENT file
-- with the same name: re-drop an edited clip under the name you used before and
-- its size or its mtime has moved, so it is a new file and is ingested. That is
-- the right way round to be wrong.
--
-- `clip_id` is the clip the file became, or NULL when the ingest FAILED — and a
-- failure is remembered exactly as firmly as a success, because a file that
-- cannot be ingested stays where the person put it. Without a row, the watcher
-- would retry it every few seconds and rewrite its `.failed.txt` for ever; with
-- one, it is decided, and `touch`ing the file (or fixing and re-saving it) makes
-- it a new key and gets it looked at again.
--
-- WHAT IT DELIBERATELY DOES NOT HOLD: no account, no path, no directory — the
-- name is a leaf, the folder is the instance's own STORAGE_DIR and there is
-- only one. It is not a history and it is not a log; it is a set of decisions
-- already taken, and the watcher prunes anything older than its retention
-- window (30 days) on the tick that writes to it, so the table stays the size
-- of a month of drops.
CREATE TABLE drop_seen (
  name    TEXT    NOT NULL,   -- the file's leaf name at the root of STORAGE_DIR
  size    INTEGER NOT NULL,   -- bytes, as the filesystem reported them
  mtime   INTEGER NOT NULL,   -- unix seconds
  at      INTEGER NOT NULL,   -- unix seconds: when this decision was taken (retention reads it)
  clip_id TEXT,               -- the clip it became; NULL when the ingest failed
  PRIMARY KEY (name, size, mtime)
);
