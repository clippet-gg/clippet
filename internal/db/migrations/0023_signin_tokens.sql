-- 0023_signin_tokens.sql — the bot's one-time login links (DESIGN.md §20
-- erratum 341: `/signin`, the bot is the door).
--
-- A member of a bound server runs `/signin`; the interaction is signed by
-- Discord and verified by the box, and the box mints ONE row here and answers
-- the member, privately, with a link carrying the raw token. The token is
-- never stored: token_hash is the hex sha256 of the raw `cs_…` value, exactly
-- as an API token's is. A row is single use — the POST that signs in DELETES it
-- in the statement that reads it — and lives five minutes (expires_at); the GET
-- that renders the page only reads it, because Discord's crawler fetches what a
-- bot says (erratum 316) and a GET must change nothing.
--
-- What the row holds is what the interaction proved and nothing more: the
-- server it was run in, the member's Discord user id (the KEY — the account it
-- opens is `users.discord_user_id = discord_user_id`, migration 0019), the
-- member's username (an unlinked member's new account is named from it) and
-- the name the page shows (`as <name>`: the global name if the member has one).
-- Snowflakes are TEXT for 0016's reason. Expired rows are removed when the next
-- token is minted; nothing else reads them.
CREATE TABLE signin_tokens (
  token_hash        TEXT PRIMARY KEY,   -- hex sha256 of the raw token
  guild_id          TEXT NOT NULL,      -- the bound server the command ran in
  discord_user_id   TEXT NOT NULL,      -- the member; the account's key
  discord_username  TEXT NOT NULL,      -- the member's username: a new account's name source
  display_name      TEXT NOT NULL,      -- the page's "as <name>": global name, else username
  created_at        INTEGER NOT NULL,   -- unix seconds
  expires_at        INTEGER NOT NULL    -- unix seconds: created_at + 300
);
CREATE INDEX signin_tokens_expires ON signin_tokens(expires_at);
