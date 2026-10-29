package store

import "context"

// AgentSeen is when a pane's agent "done" was last looked at, and of which
// agent session.
type AgentSeen struct {
	Session string
	At      int64 // (unix ms)
}

// AgentsSeen gives what was looked at of a herdr server's panes (server:
// its socket on the host), none older than since (unix ms); the older are
// let go.
func (s *Store) AgentsSeen(ctx context.Context, ownerID, hostID int64, server string, since int64) (map[string]AgentSeen, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_seen WHERE seen_at < ?`, since); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pane, session, seen_at FROM agent_seen WHERE owner_id = ? AND host_id = ? AND server = ?`, ownerID, hostID, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AgentSeen{}
	for rows.Next() {
		var pane string
		var a AgentSeen
		if err := rows.Scan(&pane, &a.Session, &a.At); err != nil {
			return nil, err
		}
		out[pane] = a
	}
	return out, rows.Err()
}

// SetAgentSeen notes a pane's agent "done" looked at.
func (s *Store) SetAgentSeen(ctx context.Context, ownerID, hostID int64, server, pane string, a AgentSeen) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_seen (owner_id, host_id, server, pane, session, seen_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner_id, host_id, server, pane) DO UPDATE SET session = excluded.session, seen_at = excluded.seen_at`, ownerID, hostID, server, pane, a.Session, a.At)
	return err
}
