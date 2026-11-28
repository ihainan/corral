-- Cached subscription quota snapshots observed passively across a user's hosts.
-- Deduplicated per (owner_id, provider, account_key). Stores non-secret quota
-- windows, display labels, source hosts, and manual-refresh rate-limit state so
-- the Subscriptions overview renders immediately without calling provider APIs.
CREATE TABLE subscription_snapshots (
  owner_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider        TEXT NOT NULL,
  account_key     TEXT NOT NULL,
  account_label   TEXT NOT NULL DEFAULT '',
  plan            TEXT NOT NULL DEFAULT '',
  host_id         INTEGER NOT NULL DEFAULT 0,
  host_name       TEXT NOT NULL DEFAULT '',
  hosts_json      TEXT NOT NULL DEFAULT '[]',
  quota_json      TEXT NOT NULL DEFAULT '{}',
  updated_at      INTEGER NOT NULL,
  seen_at         INTEGER NOT NULL,
  last_refresh_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (owner_id, provider, account_key)
);

CREATE INDEX subscription_snapshots_owner ON subscription_snapshots(owner_id, updated_at DESC);
