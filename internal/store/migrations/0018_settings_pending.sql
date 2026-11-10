-- A switch of Claude Code's model made with one line ("/model haiku") saves
-- that model as the default of new sessions; the gateway puts the default
-- back as it was. What it was is kept here from before the line is typed
-- until the line is known to be over (its log says how it ended, its
-- process is gone, or it never ran) and the default is seen settled — so
-- that a gateway started again meanwhile, a question of the line's left
-- open in the terminal, or a host that could not be written to, still has
-- it put back. One at a time for an account: its key is what the host
-- itself says it is (machine, user, the settings' file), not the address
-- it is reached by.
CREATE TABLE settings_pending (
  account    TEXT PRIMARY KEY,
  owner_id   INTEGER NOT NULL,
  host_id    INTEGER NOT NULL,
  had        INTEGER NOT NULL, -- there was a default before (0: none)
  was        BLOB NOT NULL,    -- it, as written in the settings (JSON; empty when none)
  wrote      BLOB NOT NULL,    -- what the line leaves there (JSON)
  -- Where the line was typed, to look for its end from another session too:
  mode       TEXT NOT NULL,    -- tmux or herdr
  pane       TEXT NOT NULL,
  socket     TEXT NOT NULL,    -- herdr's ('' for tmux)
  pid        INTEGER NOT NULL, -- Claude Code's process…
  start      INTEGER NOT NULL, -- …and when it started (/proc's)
  sid        TEXT NOT NULL,    -- its session's id
  log        TEXT NOT NULL,    -- its log…
  log_off    INTEGER NOT NULL, -- …and its length before the line
  created_at INTEGER NOT NULL
);
