-- Per-user configuration for automatic translation of model thinking blocks.
-- API keys are stored vault-sealed (ciphertext + wrapped_key + key_id) and never returned.
CREATE TABLE user_translate_config (
    user_id     INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    enabled     INTEGER NOT NULL DEFAULT 0,
    target_lang TEXT    NOT NULL DEFAULT 'zh-CN',
    base_url    TEXT    NOT NULL DEFAULT 'https://api.deepseek.com/v1',
    model       TEXT    NOT NULL DEFAULT 'deepseek-flash',
    ciphertext  BLOB,
    wrapped_key BLOB,
    key_id      TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL DEFAULT 0
);

-- Per-user persistent cache of translated thinking blocks, keyed by SHA-256 of
-- the source text and target language, pruned to a bounded LRU size per user.
CREATE TABLE thought_translations (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    text_hash   TEXT    NOT NULL,
    target_lang TEXT    NOT NULL,
    translated  TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, text_hash, target_lang)
);
CREATE INDEX thought_translations_lru ON thought_translations(user_id, updated_at);
