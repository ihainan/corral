-- Single sign-on (OIDC): an account bound to an identity of a provider, by the
-- provider's issuer and its subject for the person — never by an email or a
-- user name, which a provider may let people change. One identity of each
-- provider an account; one account an identity.
CREATE TABLE oidc_identities (
    issuer        TEXT    NOT NULL,
    subject       TEXT    NOT NULL,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    username_hint TEXT    NOT NULL DEFAULT '',
    email         TEXT    NOT NULL DEFAULT '',
    name          TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (issuer, subject),
    UNIQUE (user_id, issuer)
);

-- The logins made by single sign-on: which identity, and the provider's
-- session id (sid), for the provider to end them by (back-channel logout).
-- They end with their login. No token of the provider's is kept.
CREATE TABLE oidc_logins (
    token_hash TEXT PRIMARY KEY REFERENCES login_sessions(token_hash) ON DELETE CASCADE,
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    sid        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX oidc_logins_subject ON oidc_logins(issuer, subject);
CREATE INDEX oidc_logins_sid ON oidc_logins(issuer, sid);
