-- Remembered Corral agent identities for peer processes, keyed by owner, host,
-- pid, and /proc/<pid>/stat field 22 (proc_start) so PID reuse never collides.
-- Persisted when Corral observes an agent process inside a managed herdr/tmux
-- session so historical cross-session messages keep displaying the sender's
-- Corral name and location even after the sender exits or the gateway restarts.
CREATE TABLE peer_identities (
    owner_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host_id      INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    pid          INTEGER NOT NULL,
    proc_start   INTEGER NOT NULL,
    agent_key    TEXT    NOT NULL DEFAULT '',
    persistence  TEXT    NOT NULL DEFAULT '',
    mux_session  TEXT    NOT NULL DEFAULT '',
    display_name TEXT    NOT NULL DEFAULT '',
    pane         TEXT    NOT NULL DEFAULT '',
    host_name    TEXT    NOT NULL DEFAULT '',
    workspace    TEXT    NOT NULL DEFAULT '',
    title        TEXT    NOT NULL DEFAULT '',
    program      TEXT    NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (owner_id, host_id, pid, proc_start)
);

CREATE INDEX idx_peer_identities_owner_host
    ON peer_identities (owner_id, host_id, updated_at DESC);
