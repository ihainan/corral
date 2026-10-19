-- Two-step sign-in: the authenticator secret, sealed by the vault (NULL when
-- off), and the last time step used, so a code is good only once.
ALTER TABLE users ADD COLUMN totp_ciphertext  BLOB;
ALTER TABLE users ADD COLUMN totp_wrapped_key BLOB;
ALTER TABLE users ADD COLUMN totp_key_id      TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_last_step   INTEGER NOT NULL DEFAULT 0;

-- Recovery codes, by hash; one is deleted when used.
CREATE TABLE recovery_codes (
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash TEXT    NOT NULL,
    PRIMARY KEY (user_id, code_hash)
);
