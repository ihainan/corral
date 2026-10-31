-- The tmux and herdr sessions a user has open on the gateway: what is
-- opened again by itself after the gateway restarts (see web/restore.go).
-- Gone with the session (closed, ended, access revoked); kept only when the
-- gateway stops, then taken once at the next start.
CREATE TABLE open_tabs (
  session_id    TEXT PRIMARY KEY,
  owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host_id       INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL CHECK (kind IN ('tmux', 'herdr')),
  name          TEXT NOT NULL,
  herdr_default INTEGER NOT NULL DEFAULT 0,
  host_config   TEXT NOT NULL,
  cols          INTEGER NOT NULL,
  rows          INTEGER NOT NULL,
  idle_since    INTEGER NOT NULL DEFAULT 0,
  alive_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX open_tabs_logical ON open_tabs(owner_id, host_id, kind, name, host_config);
CREATE INDEX open_tabs_owner ON open_tabs(owner_id);
