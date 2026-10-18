-- Ids use AUTOINCREMENT so a deleted row's id is never handed to a new row:
-- stale references (open sessions, links) must not start pointing at someone else's data.

CREATE TABLE users (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    username            TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    display_name        TEXT    NOT NULL DEFAULT '',
    password_hash       TEXT    NOT NULL,
    role                TEXT    NOT NULL,
    disabled            INTEGER NOT NULL DEFAULT 0,
    failed_logins       INTEGER NOT NULL DEFAULT 0,
    locked_until        INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL,
    password_changed_at INTEGER NOT NULL
);

CREATE TABLE login_sessions (
    token_hash   TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    ip           TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX login_sessions_user ON login_sessions(user_id);

CREATE TABLE invites (
    token_hash TEXT    PRIMARY KEY,
    role       TEXT    NOT NULL,
    note       TEXT    NOT NULL DEFAULT '',
    created_by INTEGER NOT NULL REFERENCES users(id),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER NOT NULL DEFAULT 0,
    used_by    INTEGER
);

CREATE TABLE credentials (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    public_key  TEXT    NOT NULL DEFAULT '',
    ciphertext  BLOB    NOT NULL,
    wrapped_key BLOB    NOT NULL,
    key_id      TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX credentials_owner ON credentials(owner_id);

CREATE TABLE hosts (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name              TEXT    NOT NULL,
    address           TEXT    NOT NULL,
    port              INTEGER NOT NULL,
    username          TEXT    NOT NULL,
    auth              TEXT    NOT NULL,
    credential_id     INTEGER REFERENCES credentials(id) ON DELETE SET NULL,
    persistence       TEXT    NOT NULL DEFAULT 'tmux',
    tmux_session      TEXT    NOT NULL DEFAULT 'corral',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    last_connected_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX hosts_owner ON hosts(owner_id);

CREATE TABLE host_keys (
    host_id     INTEGER PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    key_type    TEXT    NOT NULL,
    key_data    BLOB    NOT NULL,
    fingerprint TEXT    NOT NULL,
    first_seen  INTEGER NOT NULL
);

CREATE TABLE user_settings (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    data    TEXT    NOT NULL
);

-- Append-only: nothing in the code base updates or deletes audit rows.
CREATE TABLE audit_log (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       INTEGER NOT NULL,
    user_id  INTEGER,
    username TEXT    NOT NULL DEFAULT '',
    action   TEXT    NOT NULL,
    target   TEXT    NOT NULL DEFAULT '',
    ip       TEXT    NOT NULL DEFAULT '',
    detail   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_ts ON audit_log(ts);
CREATE INDEX audit_log_user ON audit_log(user_id, ts);
