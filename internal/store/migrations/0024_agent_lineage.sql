-- Parent-child lineage edges between herdr agent panes of one herdr server
-- (server: herdrServerKey(host, socket)). Owner-scoped in SQL, keyed by
-- (child_pane, child_term_id) so recreated terminals do not inherit stale edges,
-- and updated transactionally when panes move.
CREATE TABLE agent_lineage (
  owner_id       INTEGER NOT NULL,
  host_id        INTEGER NOT NULL,
  server         TEXT    NOT NULL,
  child_pane     TEXT    NOT NULL,
  child_term_id  TEXT    NOT NULL DEFAULT '',
  child_session  TEXT    NOT NULL DEFAULT '',
  parent_pane    TEXT    NOT NULL,
  parent_term_id TEXT    NOT NULL DEFAULT '',
  parent_session TEXT    NOT NULL DEFAULT '',
  source         TEXT    NOT NULL,
  state          TEXT    NOT NULL,
  evidence_key   TEXT    NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  PRIMARY KEY (owner_id, host_id, server, child_pane, child_term_id)
);

CREATE INDEX idx_agent_lineage_scope
  ON agent_lineage (owner_id, host_id, server, updated_at);
