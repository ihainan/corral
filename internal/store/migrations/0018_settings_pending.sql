-- A switch of Claude Code's model made with one line ("/model haiku") saves
-- that model as the default of new sessions; the gateway puts the default
-- back as it was. What it was is kept here from before the line is typed
-- until it is put back, so that a gateway started again meanwhile, or a host
-- that could not be written to, still has it put back: by the account's key
-- (the host's address, port and user), one at a time.
CREATE TABLE settings_pending (
  account    TEXT PRIMARY KEY,
  owner_id   INTEGER NOT NULL,
  host_id    INTEGER NOT NULL,
  had        INTEGER NOT NULL, -- there was a default before (0: none)
  was        BLOB NOT NULL,    -- it, as written in the settings (JSON; empty when none)
  wrote      BLOB NOT NULL,    -- what the line leaves there (JSON)
  created_at INTEGER NOT NULL
);
