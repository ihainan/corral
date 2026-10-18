-- Browser push subscriptions (one per device/browser).
CREATE TABLE push_subscriptions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint    TEXT    NOT NULL UNIQUE,
    p256dh      TEXT    NOT NULL,
    auth        TEXT    NOT NULL,
    user_agent  TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    last_ok_at  INTEGER NOT NULL DEFAULT 0,
    failures    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX push_subscriptions_user ON push_subscriptions(user_id);

-- Server-wide values generated on first start (VAPID keys).
CREATE TABLE app_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Ports on a host opened in the browser through "<port>-<id>.<forward domain>".
CREATE TABLE forwards (
    id         TEXT    PRIMARY KEY,
    owner_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host_id    INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    port       INTEGER NOT NULL,
    sharing    TEXT    NOT NULL DEFAULT 'private',
    created_at INTEGER NOT NULL,
    UNIQUE (owner_id, host_id, port)
);
