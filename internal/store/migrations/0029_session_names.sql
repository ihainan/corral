-- Per-user Corral display names for remote sessions that cannot be renamed
-- on the host (herdr persistent sessions), so the name survives reconnects
-- and gateway restarts.
CREATE TABLE session_names (
    owner_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host_id      INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL,
    mux_session  TEXT    NOT NULL,
    display_name TEXT    NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (owner_id, host_id, kind, mux_session)
);
