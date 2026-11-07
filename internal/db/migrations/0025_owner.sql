-- 0025_owner.sql — the instance's OWNER: the first account created on it
-- (DESIGN.md §20 erratum 349; run 23 D3).
--
-- §14 had users and nothing else, so `/setup` and the Storage/costs panel were
-- open to any signed-in account (errata 297 (d), 270/277). The owner is the
-- account that set the instance up, and those two surfaces are now its alone.
--
-- Why a COLUMN rather than "the lowest id": ids are random text (ids.New), so
-- the lowest one is nobody in particular; and "the oldest created_at" ties at
-- one-second resolution and, worse, moves silently whenever an account is
-- deleted. A column is one durable fact an operator can read (`admin users`)
-- and move (`admin users owner <name>`).
--
-- Who gets it, and how it can never be zero or two:
--   * the backfill names the OLDEST existing account (created_at, then rowid —
--     insertion order — for a tie inside one second);
--   * users_owner_first: an INSERT into a table with no owner makes the new row
--     the owner, so the first account is the owner on EVERY path that creates
--     one — `REGISTRATION=first`/`open` registration, `admin users create`,
--     Sign in with Discord, `/signin` — with nothing in Go to forget;
--   * users_owner_next: deleting the owner (`admin users prune` is the one verb
--     that deletes a user) hands the role to the oldest remaining account, so an
--     instance with accounts always has an owner and its setup page is never
--     locked to everybody;
--   * users_one_owner: a partial UNIQUE index makes a second owner impossible.
ALTER TABLE users ADD COLUMN owner INTEGER NOT NULL DEFAULT 0 CHECK (owner IN (0, 1));
UPDATE users SET owner = 1 WHERE rowid = (SELECT rowid FROM users ORDER BY created_at, rowid LIMIT 1);
CREATE UNIQUE INDEX users_one_owner ON users(owner) WHERE owner = 1;
CREATE TRIGGER users_owner_first AFTER INSERT ON users
WHEN NOT EXISTS (SELECT 1 FROM users WHERE owner = 1)
BEGIN
  UPDATE users SET owner = 1 WHERE rowid = NEW.rowid;
END;
CREATE TRIGGER users_owner_next AFTER DELETE ON users
WHEN OLD.owner = 1
BEGIN
  UPDATE users SET owner = 1 WHERE rowid = (SELECT rowid FROM users ORDER BY created_at, rowid LIMIT 1);
END;
