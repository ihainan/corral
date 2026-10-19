-- Recorded sessions. Kept apart from the accounts they belong to (no foreign
-- keys): a recording is a record of what happened and outlives its session;
-- the owner's and host's names are copied for the same reason.
CREATE TABLE recordings (
    id         TEXT    PRIMARY KEY, -- the session id; also the file name
    owner_id   INTEGER NOT NULL,
    owner_name TEXT    NOT NULL,
    host_id    INTEGER NOT NULL,
    host_name  TEXT    NOT NULL,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER NOT NULL DEFAULT 0, -- 0 while recording
    bytes      INTEGER NOT NULL DEFAULT 0,
    truncated  INTEGER NOT NULL DEFAULT 0,
    gaps       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX recordings_started ON recordings(started_at);
CREATE INDEX recordings_owner ON recordings(owner_id, started_at);
