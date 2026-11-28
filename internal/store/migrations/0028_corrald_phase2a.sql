-- Phase 2a: daily latency rollups, persisted update notices, and changelog read cursor.

CREATE TABLE corrald_perf_daily (
  host_id  INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  day      TEXT    NOT NULL,
  op       TEXT    NOT NULL,
  via      TEXT    NOT NULL,
  count    INTEGER NOT NULL DEFAULT 0,
  total_us INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (host_id, day, op, via)
);

CREATE INDEX idx_corrald_perf_daily_day ON corrald_perf_daily(day);

CREATE TABLE corrald_update_notices (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host_id     INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  old_version TEXT    NOT NULL DEFAULT '',
  new_version TEXT    NOT NULL DEFAULT '',
  new_release INTEGER NOT NULL DEFAULT 0,
  success     INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  seen_at     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_corrald_update_notices_user_unseen ON corrald_update_notices(user_id, seen_at, created_at);

CREATE TABLE corrald_changelog_seen (
  user_id      INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  last_version TEXT    NOT NULL DEFAULT '',
  last_release INTEGER NOT NULL DEFAULT 0,
  seen_at      INTEGER NOT NULL DEFAULT 0
);
