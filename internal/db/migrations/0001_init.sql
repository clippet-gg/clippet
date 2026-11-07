-- 0001_init.sql
CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,                 -- bcrypt cost 12
  created_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  token_hash   TEXT PRIMARY KEY,               -- hex sha256 of the raw cookie value
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,           -- hex sha256 of the raw token ("ck_...")
  prefix       TEXT NOT NULL,                  -- first 8 chars after "ck_", display only
  scopes       TEXT NOT NULL DEFAULT 'upload', -- comma-separated; Phase 1 knows only 'upload'
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX api_tokens_user ON api_tokens(user_id, created_at DESC);

CREATE TABLE clips (
  id                  TEXT PRIMARY KEY,                       -- public share slug (§7)
  owner_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title               TEXT NOT NULL,
  description         TEXT NOT NULL DEFAULT '',
  unlisted            INTEGER NOT NULL DEFAULT 0,
  state               TEXT NOT NULL CHECK (state IN ('queued','processing','ready','failed','deleting')),
  failure_reason      TEXT,                                   -- set when state='failed'
  -- source (immutable after insert; "upload is sacred")
  source_key          TEXT NOT NULL,
  source_filename     TEXT NOT NULL,
  source_size         INTEGER NOT NULL,
  source_content_type TEXT NOT NULL,
  source_etag         TEXT NOT NULL,                           -- hex sha256 of the original (PutResult.SHA256 at ingest); ETag for /original
  -- rendition + poster (set at finalize; NULL until ready)
  rendition_key       TEXT,
  rendition_size      INTEGER,
  rendition_etag      TEXT,                                   -- hex sha256 of the object, computed while uploading
  poster_key          TEXT,
  poster_size         INTEGER,
  poster_etag         TEXT,
  poster_width        INTEGER,                                -- MEASURED from the JPEG (image.DecodeConfig)
  poster_height       INTEGER,
  social_key          TEXT,                                   -- NULL in Phase 1; /social.mp4 serves COALESCE(social_key, rendition_key)
  social_size         INTEGER,
  social_etag         TEXT,
  -- measured media facts (from ffprobe of the OUTPUT, not the plan)
  duration_ms         INTEGER,
  width               INTEGER,
  height              INTEGER,
  fps_num             INTEGER,
  fps_den             INTEGER,
  has_audio           INTEGER,
  -- counters + lifecycle
  views               INTEGER NOT NULL DEFAULT 0,
  ttl_seconds         INTEGER,                                -- NULL = never; resolved at upload (request or DEFAULT_CLIP_TTL); kept for Phase 4 keep-alive
  expires_at          INTEGER,                                -- NULL = never; = created_at + ttl_seconds, fixed at upload; PATCH recomputes from now
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  ready_at            INTEGER
);
CREATE INDEX clips_owner_created ON clips(owner_id, created_at DESC, id);
CREATE INDEX clips_listed        ON clips(created_at DESC, id) WHERE unlisted = 0;
CREATE INDEX clips_expires       ON clips(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX clips_state         ON clips(state);

CREATE TABLE jobs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  clip_id         TEXT NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
  kind            TEXT NOT NULL,                              -- 'transcode' in Phase 1; Phase 4: 'trim','gif','notify'
  state           TEXT NOT NULL CHECK (state IN ('queued','running','done','failed','cancelled')),
  stage           TEXT NOT NULL DEFAULT '',                   -- persisted at stage transitions (progress fallback after restart)
  attempts           INTEGER NOT NULL DEFAULT 0,              -- CRASH-class bad endings (panic, interrupted w/o checkpoint); incremented ONLY in db.FailAttempt
  transient_failures INTEGER NOT NULL DEFAULT 0,              -- ENVIRONMENT-class bad endings (storage/scratch/db/io); incremented ONLY in db.FailAttempt
  run_after       INTEGER NOT NULL,                           -- backoff gate for ClaimJob
  params_json     TEXT NOT NULL DEFAULT '{}',                 -- Phase 4 (trim points etc.)
  checkpoint_json TEXT,                                       -- set after outputs are uploaded+verified (§10 step 8); NULL before
  last_error      TEXT,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER
);
CREATE INDEX jobs_claim ON jobs(state, run_after, id);
CREATE INDEX jobs_clip  ON jobs(clip_id, id DESC);

-- Migrate creates this table before applying any file (DESIGN.md §5); IF NOT
-- EXISTS keeps both statements true on a fresh database.
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
