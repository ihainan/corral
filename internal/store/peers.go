package store

import (
	"context"
	"strings"
)

const (
	// peerIdentityRetainSeconds keeps remembered peer identities for 90 days
	// after they were last observed.
	peerIdentityRetainSeconds = 90 * 24 * 60 * 60
	// peerIdentityMaxPerHost caps rows kept per (owner_id, host_id).
	peerIdentityMaxPerHost = 2000
)

// PeerProcKey uniquely identifies a process instance on a host via its PID and
// /proc/<pid>/stat field 22 (proc_start).
type PeerProcKey struct {
	PID       int64
	ProcStart int64
}

// PeerIdentity is the persisted Corral session/pane identity for a peer agent
// process observed on a host.
type PeerIdentity struct {
	OwnerID     int64
	HostID      int64
	PID         int64
	ProcStart   int64
	AgentKey    string
	Persistence string
	MuxSession  string
	DisplayName string
	Pane        string
	HostName    string
	Workspace   string
	Title       string
	Program     string
	UpdatedAt   int64 // unix seconds
}

// SameSnapshot reports whether two PeerIdentity values carry the same
// displayable Corral target metadata (ignoring timestamps).
func (p PeerIdentity) SameSnapshot(o PeerIdentity) bool {
	return p.OwnerID == o.OwnerID &&
		p.HostID == o.HostID &&
		p.PID == o.PID &&
		p.ProcStart == o.ProcStart &&
		p.AgentKey == o.AgentKey &&
		p.Persistence == o.Persistence &&
		p.MuxSession == o.MuxSession &&
		p.DisplayName == o.DisplayName &&
		p.Pane == o.Pane &&
		p.HostName == o.HostName &&
		p.Workspace == o.Workspace &&
		p.Title == o.Title &&
		p.Program == o.Program
}

// UpsertPeerIdentities persists a batch of observed peer agent identities for
// one owner and host, and opportunistically prunes stale rows beyond retention.
func (s *Store) UpsertPeerIdentities(ctx context.Context, ownerID, hostID int64, rows []PeerIdentity) error {
	if ownerID <= 0 || hostID <= 0 || len(rows) == 0 {
		return nil
	}
	now := s.unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO peer_identities (
		owner_id, host_id, pid, proc_start, agent_key, persistence, mux_session,
		display_name, pane, host_name, workspace, title, program, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(owner_id, host_id, pid, proc_start) DO UPDATE SET
		agent_key    = excluded.agent_key,
		persistence  = excluded.persistence,
		mux_session  = excluded.mux_session,
		display_name = excluded.display_name,
		pane         = excluded.pane,
		host_name    = excluded.host_name,
		workspace    = excluded.workspace,
		title        = CASE WHEN excluded.title != '' THEN excluded.title ELSE peer_identities.title END,
		program      = CASE WHEN excluded.program != '' THEN excluded.program ELSE peer_identities.program END,
		updated_at   = excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if r.PID <= 0 || r.ProcStart <= 0 {
			continue
		}
		ts := r.UpdatedAt
		if ts <= 0 {
			ts = now
		}
		if _, err := stmt.ExecContext(ctx,
			ownerID, hostID, r.PID, r.ProcStart, r.AgentKey, r.Persistence, r.MuxSession,
			r.DisplayName, r.Pane, r.HostName, r.Workspace, r.Title, r.Program, ts,
		); err != nil {
			return err
		}
	}

	cutoff := now - peerIdentityRetainSeconds
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM peer_identities WHERE owner_id = ? AND host_id = ? AND updated_at < ?`,
		ownerID, hostID, cutoff,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM peer_identities
		 WHERE owner_id = ? AND host_id = ?
		   AND (pid, proc_start) NOT IN (
		     SELECT pid, proc_start FROM peer_identities
		     WHERE owner_id = ? AND host_id = ?
		     ORDER BY updated_at DESC, pid DESC, proc_start DESC
		     LIMIT ?
		   )`,
		ownerID, hostID, ownerID, hostID, peerIdentityMaxPerHost,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// LookupPeerIdentities loads persisted peer identities for a set of
// (pid, proc_start) keys scoped strictly to (ownerID, hostID).
func (s *Store) LookupPeerIdentities(ctx context.Context, ownerID, hostID int64, keys []PeerProcKey) (map[PeerProcKey]PeerIdentity, error) {
	if ownerID <= 0 || hostID <= 0 || len(keys) == 0 {
		return nil, nil
	}
	// Deduplicate valid keys and chunk to stay well within SQLite's bind limit.
	seen := make(map[PeerProcKey]struct{}, len(keys))
	unique := make([]PeerProcKey, 0, len(keys))
	for _, k := range keys {
		if k.PID <= 0 || k.ProcStart <= 0 {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		unique = append(unique, k)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	out := make(map[PeerProcKey]PeerIdentity, len(unique))
	const chunkSize = 200
	for start := 0; start < len(unique); start += chunkSize {
		end := start + chunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]
		var b strings.Builder
		b.WriteString(`SELECT owner_id, host_id, pid, proc_start, agent_key, persistence, mux_session,
			display_name, pane, host_name, workspace, title, program, updated_at
			FROM peer_identities
			WHERE owner_id = ? AND host_id = ? AND (`)
		args := make([]any, 0, 2+len(chunk)*2)
		args = append(args, ownerID, hostID)
		for i, k := range chunk {
			if i > 0 {
				b.WriteString(" OR ")
			}
			b.WriteString("(pid = ? AND proc_start = ?)")
			args = append(args, k.PID, k.ProcStart)
		}
		b.WriteString(")")

		rows, err := s.db.QueryContext(ctx, b.String(), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r PeerIdentity
			if err := rows.Scan(
				&r.OwnerID, &r.HostID, &r.PID, &r.ProcStart, &r.AgentKey, &r.Persistence, &r.MuxSession,
				&r.DisplayName, &r.Pane, &r.HostName, &r.Workspace, &r.Title, &r.Program, &r.UpdatedAt,
			); err != nil {
				rows.Close()
				return nil, err
			}
			out[PeerProcKey{PID: r.PID, ProcStart: r.ProcStart}] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
