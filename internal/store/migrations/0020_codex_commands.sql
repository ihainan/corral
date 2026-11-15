-- Persist acceptance uncertainty across gateway restarts. Message text and
-- credentials are not stored here; digest prevents reusing a key for new input.
CREATE TABLE codex_commands (
    owner_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    command_key TEXT NOT NULL,
    digest TEXT NOT NULL,
    status INTEGER NOT NULL DEFAULT 409,
    result TEXT NOT NULL DEFAULT '{"error":"outcome_unknown"}',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (owner_id, host_id, command_key)
);
