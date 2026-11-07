-- 0019_discord_login.sql — which Discord user may open an account (ROADMAP #44,
-- DESIGN.md §20 erratum 302).
--
-- A Discord login is a SECOND KEY to an existing door, never a door: the
-- callback looks the Discord user id up here and issues a session for the
-- account it finds, and creates nothing when it finds none — under any
-- REGISTRATION value. The link is written by exactly two hands, both explicit:
-- the signed-in owner from Settings (with their password), and the operator
-- with `clippet admin users link <username> <discord-id>`. Nothing else reads
-- it: the bot's guild binding (0016) is a separate table answering a separate
-- question, and there are no global users and nothing cross-instance.
--
-- discord_user_id is the snowflake as TEXT for 0016's reason: a 64-bit id
-- that JSON and JavaScript cannot hold, spelled one way everywhere it travels.
-- NULL = not linked. The display name is refreshed at every Discord login and
-- decides nothing — Discord usernames are mutable and not unique across time;
-- the id is the key.
--
-- A PARTIAL unique index rather than a UNIQUE column: any number of unlinked
-- accounts coexist (NULLs are distinct in SQLite's UNIQUE anyway, but the
-- partial index says the intent), and it is the read path's index. A deleted
-- account takes its link with it — the columns are on the row.
ALTER TABLE users ADD COLUMN discord_user_id   TEXT;                        -- snowflake as text; NULL = not linked
ALTER TABLE users ADD COLUMN discord_username  TEXT NOT NULL DEFAULT '';    -- display only
ALTER TABLE users ADD COLUMN discord_linked_at INTEGER;                     -- unix seconds
CREATE UNIQUE INDEX users_discord ON users(discord_user_id) WHERE discord_user_id IS NOT NULL;
