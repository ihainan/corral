-- Phase 2b: per-(user, host) contextual corrald hint tracking (shown / dismissed).

CREATE TABLE corrald_host_hints (
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host_id      INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  shown_at     INTEGER NOT NULL DEFAULT 0,
  dismissed_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, host_id)
);

CREATE INDEX idx_corrald_host_hints_host ON corrald_host_hints(host_id);
