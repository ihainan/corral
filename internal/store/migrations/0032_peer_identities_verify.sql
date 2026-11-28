-- Add verified pane/session identity fields and last_seen retention timestamp
-- to peer_identities so replaced panes never misattribute old messages to a
-- newer agent and continuously observed processes extend their 90-day window.
ALTER TABLE peer_identities ADD COLUMN agent_session TEXT NOT NULL DEFAULT '';
ALTER TABLE peer_identities ADD COLUMN terminal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE peer_identities ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;

UPDATE peer_identities SET last_seen = updated_at WHERE last_seen = 0;

CREATE INDEX idx_peer_identities_owner_host_seen
    ON peer_identities (owner_id, host_id, last_seen DESC);
