-- 0016_discord_guilds.sql — which account a Discord server's `/clip` uploads
-- into (ROADMAP #33, DESIGN.md §20 erratum 255).
--
-- Clippet is single-tenant (§1): one instance, one operator, one set of
-- accounts. The bot does not change that — it adds one mapping from a Discord
-- server to an account that already exists, written by the operator with
-- `clippet admin bot bind <guild-id> <username>`. There is no self-service
-- path, no OAuth flow and no per-user linking, because a guild the operator
-- did not name has no business writing into anybody's library: an unbound
-- guild gets a one-line refusal and nothing is created.
--
-- ON DELETE CASCADE matches `clips.owner_id` exactly (0001_init.sql). The
-- alternative — refusing to delete a bound account — would make `admin users
-- prune` fail on a row the operator cannot see from the users listing, and the
-- binding is not data anybody loses sleep over: re-running `admin bot bind`
-- restores it in one command. Note the consequence, which DEPLOY §30 states:
-- deleting an account silently unbinds every guild that pointed at it, and the
-- next `/clip` there is refused as unbound rather than uploading somewhere
-- unexpected. That is the safe direction.
--
-- guild_id is Discord's snowflake as a TEXT string — it is a 64-bit unsigned
-- id, which SQLite's INTEGER can hold but JSON and JavaScript cannot, and
-- every place it travels (the interaction body, the CLI, the logs) it is a
-- string. Keeping one spelling means no conversion can ever disagree.
--
-- No index beyond the primary key: the read path is `WHERE guild_id = ?` (one
-- lookup per interaction) and the listing is the whole table, which is one row
-- per Discord server the operator runs — a handful, ever.
CREATE TABLE discord_guilds (
  guild_id   TEXT PRIMARY KEY,                                   -- Discord's server snowflake, as text
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL                                    -- unix seconds; when the operator bound it
);
