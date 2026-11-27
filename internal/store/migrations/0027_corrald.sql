CREATE TABLE corrald_hosts (
  host_id           INTEGER PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
  state             TEXT NOT NULL DEFAULT 'off',
  consent_by        INTEGER,
  consent_at        INTEGER NOT NULL DEFAULT 0,
  bound_fingerprint TEXT NOT NULL DEFAULT '',
  bound_username    TEXT NOT NULL DEFAULT '',
  bound_gateway     TEXT NOT NULL DEFAULT '',
  manifest_json     TEXT NOT NULL DEFAULT '',
  version           TEXT NOT NULL DEFAULT '',
  release           INTEGER NOT NULL DEFAULT 0,
  machine_id        TEXT NOT NULL DEFAULT '',
  socket_path       TEXT NOT NULL DEFAULT '',
  files_json        TEXT NOT NULL DEFAULT '[]',
  paused_users_json TEXT NOT NULL DEFAULT '[]',
  last_ok_at        INTEGER NOT NULL DEFAULT 0,
  last_error        TEXT NOT NULL DEFAULT '',
  restart_count     INTEGER NOT NULL DEFAULT 0,
  updated_at        INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_corrald_hosts_binding ON corrald_hosts(bound_fingerprint, bound_username);

CREATE TABLE corrald_ops (
  id         TEXT PRIMARY KEY,
  host_id    INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,
  state      TEXT NOT NULL,
  step       TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '',
  created_by INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX idx_corrald_ops_host_created ON corrald_ops(host_id, created_at DESC);
CREATE UNIQUE INDEX idx_corrald_ops_active ON corrald_ops(host_id) WHERE state IN ('pending', 'running');
