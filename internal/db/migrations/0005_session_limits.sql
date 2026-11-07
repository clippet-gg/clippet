-- 0005_session_limits.sql — account security (§17 #1/#21/#25, §20 errata 105–112).
--
-- ONE column and ONE index. `api_tokens.scopes` already exists (0001 created it
-- as a comma-separated TEXT column defaulting to 'upload'), so the read scope
-- of §20 erratum 111 needs no schema change at all — only a wider set of values
-- written into a column that was designed for exactly this.
--
-- sessions.max_expires_at is the CEILING on the sliding window (§20 erratum
-- 105). `expires_at` alone made an actively used session immortal: every
-- request past the once-a-minute gate pushed it another SESSION_TTL into the
-- future, so a stolen `SESSION_TTL=720h` cookie that keeps being used never
-- expires and the only revocation surface was the very credential being
-- stolen. The cap is measured from CREATION and is never moved by a touch, so
-- it is a fact about the session and not about its traffic.
--
-- 0 means "no ceiling recorded". No row ever carries it after this migration —
-- the UPDATE below backfills every existing session and CreateSession always
-- writes one — but the column is read defensively (`max_expires_at > 0`) so a
-- row written by an older binary against a migrated file cannot be shortened
-- to the epoch. That is the safe direction to be wrong in: a missing ceiling
-- degrades to the behaviour that shipped before this migration, while a zero
-- read as a deadline would log every existing user out at once.
ALTER TABLE sessions ADD COLUMN max_expires_at INTEGER NOT NULL DEFAULT 0;

-- The backfill uses the DEFAULT ninety days rather than the running config: a
-- migration has no configuration, and an existing session's honest ceiling is
-- "ninety days from when it was created", which for anything older than that
-- lands in the past and ends the session at its next touch. That is the
-- intended effect — those are exactly the sessions the cap exists for.
UPDATE sessions SET max_expires_at = created_at + 7776000 WHERE max_expires_at = 0;

-- "Every live session of this user, newest first" is the active-session list
-- (GET /api/auth/sessions), the log-out-everywhere-else DELETE, the password
-- change's session sweep and `clippet admin sessions revoke <username>`. Four
-- statements over one shape; without this index each is a full table scan of
-- `sessions`, which on a shared instance is every browser of every account.
CREATE INDEX sessions_user ON sessions(user_id, created_at DESC);
