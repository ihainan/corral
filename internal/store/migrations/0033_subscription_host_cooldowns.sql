-- Per-(owner, host, provider) manual-refresh cooldown ledger. Enforced before
-- remote provider commands (e.g. OpenCode) in addition to per-account cooldowns
-- so workspace switching, rejected refreshes, or placeholder cards cannot
-- bypass rate limits.
CREATE TABLE subscription_host_cooldowns (
  owner_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host_id         INTEGER NOT NULL,
  provider        TEXT NOT NULL,
  last_refresh_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (owner_id, host_id, provider)
);
