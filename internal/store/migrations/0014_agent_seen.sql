-- An agent "done" looked at on the agents page: by pane of a herdr server
-- (server: which machine and socket), the agent session and when. Kept so that a gateway started again (or a
-- session reconnected) does not show it "done" anew: a round over that
-- began after it was looked at is not this one.
CREATE TABLE agent_seen (
  owner_id INTEGER NOT NULL,
  host_id  INTEGER NOT NULL,
  server   TEXT NOT NULL,
  pane     TEXT NOT NULL,
  session  TEXT NOT NULL,
  seen_at  INTEGER NOT NULL,
  PRIMARY KEY (owner_id, host_id, server, pane)
);
