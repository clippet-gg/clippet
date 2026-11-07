-- 0013_sweep_failures.sql — the sweeper's per-clip failure backoff, on disk
-- (N004; DESIGN.md §12, §20 erratum 232, which amends erratum 101(a)).
--
-- Erratum 101(a) gave the sweeper an in-memory `clipID → {failures, next
-- attempt}` map so two black-holing prefixes could not consume every tick and
-- starve TTL, and said the map was deliberately NOT persisted: a restart is
-- the one event that plausibly fixes storage, so forgetting the schedule then
-- is right. That is true of a restart an operator performs. It is exactly
-- wrong for the restart nobody performed: `restart: unless-stopped` plus a
-- process that dies every few minutes (an OOM kill during an encode, a host
-- reboot loop) resets the schedule on every boot, and the sweeper then
-- re-attempts the same failing prefix at full cost — up to STORAGE_OP_TIMEOUT
-- per call — for as long as the loop lasts. This table is that memory.
--
-- One row per FAILING clip and nothing else: the population is the clips whose
-- Destroy is currently failing, which is normally zero rows and is bounded in
-- memory at 1024. A successful (or skipped) Destroy deletes the row, and the
-- foreign key means a clip that leaves by any other door takes its row with
-- it, so nothing here can outlive the thing it describes.
--
-- `failures` is the consecutive-failure count the backoff is computed from
-- (2m·2^(n-1), capped at 1h — sweeper.backoffFor, unchanged), `next_attempt_at`
-- is that schedule made explicit so a process that did not compute it still
-- honours it, and `last_attempt_at` is the observable an operator (or a test)
-- reads to tell "backing off since the crash" from "never attempted". All
-- three are unix seconds, from the same clock the sweeper ticks on.
--
-- No index: every read is either the primary key or the whole table, which is
-- read exactly once per process (at the first tick) and is bounded by the same
-- 1024 the in-memory map is.
CREATE TABLE sweep_failures (
  clip_id         TEXT PRIMARY KEY REFERENCES clips(id) ON DELETE CASCADE,
  failures        INTEGER NOT NULL,                                   -- consecutive failed Destroys, ≥ 1
  last_attempt_at INTEGER NOT NULL,                                   -- unix seconds of the failure that wrote this row
  next_attempt_at INTEGER NOT NULL                                    -- unix seconds: earliest next attempt
);
