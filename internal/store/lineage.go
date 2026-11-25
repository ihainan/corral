package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const (
	LineageManual      = "manual"
	LineageCorralSpawn = "corral_spawn"
	LineageMetadata    = "metadata"
	LineageToolLog     = "tool_log"

	LineageConfirmed = "confirmed"
	LineageRejected  = "rejected"

	lineageMaxDepth = 16
)

var (
	ErrLineageCycle   = errors.New("lineage cycle detected")
	ErrLineageInvalid = errors.New("invalid lineage edge")
)

// AgentLineage is one persisted parent-child relation (or manual rejection
// tombstone) between two herdr panes within a single owner, host, and herdr
// server scope.
type AgentLineage struct {
	OwnerID       int64
	HostID        int64
	Server        string
	ChildPane     string
	ChildTermID   string
	ChildSession  string
	ParentPane    string
	ParentTermID  string
	ParentSession string
	Source        string
	State         string
	EvidenceKey   string
	CreatedAt     int64 // unix ms
	UpdatedAt     int64 // unix ms
}

func lineagePriority(source string) int {
	switch source {
	case LineageManual:
		return 4
	case LineageCorralSpawn:
		return 3
	case LineageMetadata:
		return 2
	case LineageToolLog:
		return 1
	default:
		return 0
	}
}

func validLineageSource(source string) bool {
	return lineagePriority(source) > 0
}

// ListAgentLineage returns all persisted lineage rows (confirmed and rejected)
// for one owner, host, and herdr server.
func (s *Store) ListAgentLineage(ctx context.Context, ownerID, hostID int64, server string) ([]AgentLineage, error) {
	return listAgentLineageQ(ctx, s.db, ownerID, hostID, server)
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listAgentLineageQ(ctx context.Context, q queryer, ownerID, hostID int64, server string) ([]AgentLineage, error) {
	rows, err := q.QueryContext(ctx, `SELECT owner_id, host_id, server, child_pane, child_term_id, child_session,
		parent_pane, parent_term_id, parent_session, source, state, evidence_key, created_at, updated_at
		FROM agent_lineage
		WHERE owner_id = ? AND host_id = ? AND server = ?
		ORDER BY created_at ASC, child_pane ASC, child_term_id ASC`, ownerID, hostID, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentLineage
	for rows.Next() {
		var r AgentLineage
		if err := rows.Scan(&r.OwnerID, &r.HostID, &r.Server, &r.ChildPane, &r.ChildTermID, &r.ChildSession,
			&r.ParentPane, &r.ParentTermID, &r.ParentSession, &r.Source, &r.State, &r.EvidenceKey,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneRetiredAgentLineage deletes lineage rows older than olderThan (unix ms)
// whose child_term_id is NOT currently alive in aliveTermIDs. Active instances
// (including rejected tombstones for live terminals) are never pruned while
// alive.
func (s *Store) PruneRetiredAgentLineage(ctx context.Context, ownerID, hostID int64, server string, olderThan int64, aliveTermIDs map[string]bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := listAgentLineageQ(ctx, tx, ownerID, hostID, server)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.UpdatedAt >= olderThan {
			continue
		}
		if r.ChildTermID != "" && aliveTermIDs[r.ChildTermID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_lineage
			WHERE owner_id = ? AND host_id = ? AND server = ? AND child_pane = ? AND child_term_id = ?`,
			ownerID, hostID, server, r.ChildPane, r.ChildTermID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertAgentLineage transactionally validates and persists a confirmed
// parent-child edge. It enforces:
//   - rejected stickiness (non-manual sources never resurrect a rejected edge,
//     including empty-term upgrade collisions)
//   - source priority (manual > corral_spawn > metadata > tool_log) and
//     first-owner preservation on equal priority
//   - evidence_key first-owner guard (forked logs cannot steal existing children)
//   - strict cycle check (depth >= 16 without reaching a root is rejected)
//   - idempotency (returns changed=false without writing when nothing changed)
func (s *Store) UpsertAgentLineage(ctx context.Context, row AgentLineage) (bool, error) {
	if row.OwnerID <= 0 || row.HostID <= 0 || row.Server == "" || row.ChildPane == "" || row.ParentPane == "" || !validLineageSource(row.Source) {
		return false, ErrLineageInvalid
	}
	if row.ChildPane == row.ParentPane {
		return false, ErrLineageCycle
	}
	if row.ChildTermID != "" && row.ParentTermID != "" && row.ChildTermID == row.ParentTermID {
		return false, ErrLineageCycle
	}
	if row.State == "" {
		row.State = LineageConfirmed
	}
	if row.State != LineageConfirmed {
		return false, ErrLineageInvalid
	}
	now := row.UpdatedAt
	if now <= 0 {
		now = s.now().UnixMilli()
	}
	if row.CreatedAt <= 0 {
		row.CreatedAt = now
	}
	row.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	all, err := listAgentLineageQ(ctx, tx, row.OwnerID, row.HostID, row.Server)
	if err != nil {
		return false, err
	}

	isManual := row.Source == LineageManual
	var existing *AgentLineage
	var upgradeFrom *AgentLineage

	for i := range all {
		cur := &all[i]
		sameChild := cur.ChildPane == row.ChildPane && cur.ChildTermID == row.ChildTermID
		sameTerm := row.ChildTermID != "" && cur.ChildTermID == row.ChildTermID
		emptyTermSamePane := cur.ChildPane == row.ChildPane && cur.ChildTermID == ""
		incomingEmptySamePane := row.ChildTermID == "" && cur.ChildPane == row.ChildPane

		if cur.State == LineageRejected {
			if !isManual && (sameChild || sameTerm || emptyTermSamePane || incomingEmptySamePane) {
				return false, nil
			}
			continue
		}

		if cur.State != LineageConfirmed {
			continue
		}

		if sameChild || sameTerm {
			existing = cur
		} else if row.ChildTermID != "" && emptyTermSamePane && upgradeFrom == nil {
			upgradeFrom = cur
		} else if incomingEmptySamePane && existing == nil {
			existing = cur
		}

		// Evidence key first-owner guard: prevent a forked log from hijacking
		// an already-recorded creation evidence key.
		if !isManual && row.EvidenceKey != "" && cur.EvidenceKey == row.EvidenceKey {
			sameParent := cur.ParentPane == row.ParentPane &&
				(cur.ParentTermID == "" || row.ParentTermID == "" || cur.ParentTermID == row.ParentTermID)
			if !sameParent || (!sameChild && !sameTerm && !emptyTermSamePane && !incomingEmptySamePane) {
				return false, nil
			}
		}
	}

	if existing == nil && upgradeFrom != nil {
		existing = upgradeFrom
	}

	var deleteKey *[2]string
	merged := row

	if existing != nil {
		sameParent := existing.ParentPane == row.ParentPane &&
			(existing.ParentTermID == "" || row.ParentTermID == "" || existing.ParentTermID == row.ParentTermID)
		oldPrio := lineagePriority(existing.Source)
		newPrio := lineagePriority(row.Source)

		if !isManual {
			// Never rebind an existing confirmed edge onto a different child session,
			// even if a higher-priority automatic source proposes another parent.
			if existing.ChildSession != "" && row.ChildSession != "" && existing.ChildSession != row.ChildSession {
				return false, nil
			}
			if sameParent && existing.ParentSession != "" && row.ParentSession != "" && existing.ParentSession != row.ParentSession {
				return false, nil
			}
			if oldPrio > newPrio {
				if !sameParent {
					return false, nil
				}
				// Same parent from a lower-priority source: keep higher source, only
				// enrich missing identity fields.
				merged.Source = existing.Source
			} else if oldPrio == newPrio && !sameParent {
				// First-owner wins on equal priority.
				return false, nil
			}
		}

		if sameParent {
			// Preserve already-bound non-empty identity fields; only fill missing ones. A manual link is made
			// against the panes as they are now (the handler read their live identity), so its own non-empty
			// fields win: relinking a child whose agent session changed under the same parent must bind the new
			// session, or the edge stays hidden while the person is told it was made.
			keep := func(old, cur string) string {
				if old != "" && (!isManual || cur == "") {
					return old
				}
				return cur
			}
			merged.ChildTermID = keep(existing.ChildTermID, merged.ChildTermID)
			merged.ChildSession = keep(existing.ChildSession, merged.ChildSession)
			merged.ParentTermID = keep(existing.ParentTermID, merged.ParentTermID)
			merged.ParentSession = keep(existing.ParentSession, merged.ParentSession)
			if merged.EvidenceKey == "" {
				merged.EvidenceKey = existing.EvidenceKey
			}
			merged.CreatedAt = existing.CreatedAt
		} else {
			// Manual or strictly higher-priority source replacing parent.
			if existing.ChildTermID != "" && !isManual {
				merged.ChildTermID = existing.ChildTermID
			} else if merged.ChildTermID == "" {
				merged.ChildTermID = existing.ChildTermID
			}
			if existing.ChildSession != "" && !isManual {
				merged.ChildSession = existing.ChildSession
			} else if merged.ChildSession == "" {
				merged.ChildSession = existing.ChildSession
			}
			merged.CreatedAt = existing.CreatedAt
		}

		// Idempotency: if nothing meaningful changed, skip the write so we don't
		// churn updated_at on repeated snapshots.
		if existing.ChildPane == merged.ChildPane &&
			existing.ChildTermID == merged.ChildTermID &&
			existing.ChildSession == merged.ChildSession &&
			existing.ParentPane == merged.ParentPane &&
			existing.ParentTermID == merged.ParentTermID &&
			existing.ParentSession == merged.ParentSession &&
			existing.Source == merged.Source &&
			existing.State == merged.State &&
			existing.EvidenceKey == merged.EvidenceKey {
			return false, nil
		}

		if existing.ChildPane != merged.ChildPane || existing.ChildTermID != merged.ChildTermID {
			k := [2]string{existing.ChildPane, existing.ChildTermID}
			deleteKey = &k
		}
	}

	// Cycle check against the post-write confirmed graph.
	if hasLineageCycle(all, existing, merged) {
		return false, ErrLineageCycle
	}

	if deleteKey != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_lineage
			WHERE owner_id = ? AND host_id = ? AND server = ? AND child_pane = ? AND child_term_id = ?`,
			row.OwnerID, row.HostID, row.Server, deleteKey[0], deleteKey[1]); err != nil {
			return false, err
		}
	}
	// If manual link overrides a rejected tombstone for the same pane with empty term,
	// clear that tombstone too so the new manual edge is unambiguous.
	if isManual && merged.ChildTermID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_lineage
			WHERE owner_id = ? AND host_id = ? AND server = ? AND child_pane = ? AND child_term_id = '' AND state = ?`,
			row.OwnerID, row.HostID, row.Server, merged.ChildPane, LineageRejected); err != nil {
			return false, err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_lineage (
		owner_id, host_id, server, child_pane, child_term_id, child_session,
		parent_pane, parent_term_id, parent_session, source, state, evidence_key,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(owner_id, host_id, server, child_pane, child_term_id) DO UPDATE SET
		child_session = excluded.child_session,
		parent_pane = excluded.parent_pane,
		parent_term_id = excluded.parent_term_id,
		parent_session = excluded.parent_session,
		source = excluded.source,
		state = excluded.state,
		evidence_key = excluded.evidence_key,
		updated_at = excluded.updated_at`,
		merged.OwnerID, merged.HostID, merged.Server,
		merged.ChildPane, merged.ChildTermID, merged.ChildSession,
		merged.ParentPane, merged.ParentTermID, merged.ParentSession,
		merged.Source, merged.State, merged.EvidenceKey,
		merged.CreatedAt, merged.UpdatedAt); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// UpgradeAgentLineageIdentity safely fills missing terminal/session identity
// fields on an existing CONFIRMED row inside a transaction. Unlike
// UpsertAgentLineage(Source: manual), it NEVER resurrects a rejected row, NEVER
// changes parent or source, and NEVER overwrites already-bound non-empty
// identity fields.
func (s *Store) UpgradeAgentLineageIdentity(ctx context.Context, row AgentLineage) (bool, error) {
	if row.OwnerID <= 0 || row.HostID <= 0 || row.Server == "" || row.ChildPane == "" || row.ParentPane == "" {
		return false, ErrLineageInvalid
	}
	now := row.UpdatedAt
	if now <= 0 {
		now = s.now().UnixMilli()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	all, err := listAgentLineageQ(ctx, tx, row.OwnerID, row.HostID, row.Server)
	if err != nil {
		return false, err
	}

	var existing *AgentLineage
	for i := range all {
		cur := &all[i]
		sameChild := cur.ChildPane == row.ChildPane && cur.ChildTermID == row.ChildTermID
		sameTerm := row.ChildTermID != "" && cur.ChildTermID == row.ChildTermID
		emptyTermSamePane := cur.ChildPane == row.ChildPane && cur.ChildTermID == ""

		if cur.State == LineageRejected {
			if sameChild || sameTerm || emptyTermSamePane {
				return false, nil
			}
			continue
		}
		if cur.State != LineageConfirmed {
			continue
		}
		if sameChild || sameTerm {
			existing = cur
		} else if row.ChildTermID != "" && emptyTermSamePane && existing == nil {
			existing = cur
		}
	}
	if existing == nil || existing.ParentPane != row.ParentPane {
		return false, nil
	}
	if existing.ChildTermID != "" && row.ChildTermID != "" && existing.ChildTermID != row.ChildTermID {
		return false, nil
	}
	if existing.ChildSession != "" && row.ChildSession != "" && existing.ChildSession != row.ChildSession {
		return false, nil
	}
	if existing.ParentTermID != "" && row.ParentTermID != "" && existing.ParentTermID != row.ParentTermID {
		return false, nil
	}
	if existing.ParentSession != "" && row.ParentSession != "" && existing.ParentSession != row.ParentSession {
		return false, nil
	}

	merged := *existing
	if merged.ChildTermID == "" {
		merged.ChildTermID = row.ChildTermID
	}
	if merged.ChildSession == "" {
		merged.ChildSession = row.ChildSession
	}
	if merged.ParentTermID == "" {
		merged.ParentTermID = row.ParentTermID
	}
	if merged.ParentSession == "" {
		merged.ParentSession = row.ParentSession
	}
	if merged.ChildTermID == existing.ChildTermID &&
		merged.ChildSession == existing.ChildSession &&
		merged.ParentTermID == existing.ParentTermID &&
		merged.ParentSession == existing.ParentSession {
		return false, nil
	}
	merged.UpdatedAt = now

	if hasLineageCycle(all, existing, merged) {
		return false, ErrLineageCycle
	}

	if existing.ChildTermID != merged.ChildTermID {
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_lineage
			WHERE owner_id = ? AND host_id = ? AND server = ? AND child_pane = ? AND child_term_id = ?`,
			row.OwnerID, row.HostID, row.Server, existing.ChildPane, existing.ChildTermID); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_lineage (
		owner_id, host_id, server, child_pane, child_term_id, child_session,
		parent_pane, parent_term_id, parent_session, source, state, evidence_key,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(owner_id, host_id, server, child_pane, child_term_id) DO UPDATE SET
		child_session = excluded.child_session,
		parent_term_id = excluded.parent_term_id,
		parent_session = excluded.parent_session,
		updated_at = excluded.updated_at`,
		merged.OwnerID, merged.HostID, merged.Server,
		merged.ChildPane, merged.ChildTermID, merged.ChildSession,
		merged.ParentPane, merged.ParentTermID, merged.ParentSession,
		merged.Source, merged.State, merged.EvidenceKey,
		merged.CreatedAt, merged.UpdatedAt); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// hasLineageCycle returns true if inserting/updating candidate creates a cycle
// or an ancestor chain of depth >= lineageMaxDepth. Terminal-bound edges never
// traverse retired historical edges with a different child_term_id.
func hasLineageCycle(all []AgentLineage, replacing *AgentLineage, candidate AgentLineage) bool {
	if candidate.ChildPane == candidate.ParentPane {
		return true
	}
	if candidate.ChildTermID != "" && candidate.ParentTermID != "" && candidate.ChildTermID == candidate.ParentTermID {
		return true
	}

	type edge struct {
		childTerm  string
		parentPane string
		parentTerm string
	}
	byTerm := map[string]edge{}
	byPaneUntyped := map[string]edge{}
	byPaneAny := map[string]edge{}

	add := func(r AgentLineage) {
		e := edge{childTerm: r.ChildTermID, parentPane: r.ParentPane, parentTerm: r.ParentTermID}
		if r.ChildTermID != "" {
			byTerm[r.ChildTermID] = e
		} else {
			byPaneUntyped[r.ChildPane] = e
		}
		byPaneAny[r.ChildPane] = e
	}
	for _, r := range all {
		if r.State != LineageConfirmed {
			continue
		}
		if replacing != nil && r.ChildPane == replacing.ChildPane && r.ChildTermID == replacing.ChildTermID {
			continue
		}
		add(r)
	}
	add(candidate)

	curPane := candidate.ParentPane
	curTerm := candidate.ParentTermID
	for depth := 1; depth < lineageMaxDepth; depth++ {
		if candidate.ChildTermID != "" && curTerm != "" {
			if curTerm == candidate.ChildTermID {
				return true
			}
		} else if curPane == candidate.ChildPane {
			return true
		}
		var next edge
		var ok bool
		if curTerm != "" {
			next, ok = byTerm[curTerm]
			if !ok && curPane != "" {
				next, ok = byPaneUntyped[curPane]
			}
		} else if curPane != "" {
			next, ok = byPaneAny[curPane]
		}
		if !ok {
			return false
		}
		curPane, curTerm = next.parentPane, next.parentTerm
	}
	// Exceeded max depth without reaching a root: treat as a cycle/unbounded chain.
	return true
}

// RejectAgentLineage marks a child pane/terminal as manually unlinked so
// automatic collectors (tool_log, metadata) will not re-attach it.
func (s *Store) RejectAgentLineage(ctx context.Context, ownerID, hostID int64, server, childPane, childTermID, childSession string, now int64) error {
	if ownerID <= 0 || hostID <= 0 || server == "" || childPane == "" {
		return ErrLineageInvalid
	}
	if now <= 0 {
		now = s.now().UnixMilli()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Remove any empty-term row for this pane when we have a concrete term_id,
	// or remove matching term_id rows under an old pane name.
	if childTermID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_lineage
			WHERE owner_id = ? AND host_id = ? AND server = ?
			  AND ((child_pane = ? AND child_term_id = '') OR (child_term_id = ? AND child_pane != ?))`,
			ownerID, hostID, server, childPane, childTermID, childPane); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_lineage (
		owner_id, host_id, server, child_pane, child_term_id, child_session,
		parent_pane, parent_term_id, parent_session, source, state, evidence_key,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, '', '', '', ?, ?, '', ?, ?)
	ON CONFLICT(owner_id, host_id, server, child_pane, child_term_id) DO UPDATE SET
		child_session = CASE WHEN excluded.child_session != '' THEN excluded.child_session ELSE agent_lineage.child_session END,
		parent_pane = '',
		parent_term_id = '',
		parent_session = '',
		source = excluded.source,
		state = excluded.state,
		evidence_key = '',
		updated_at = excluded.updated_at`,
		ownerID, hostID, server, childPane, childTermID, childSession,
		LineageManual, LineageRejected, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MigrateAgentLineagePanes updates child_pane and parent_pane across ALL rows
// (both confirmed and rejected) whenever a live terminal_id is observed at a
// new pane_id (e.g. after pane.moved).
func (s *Store) MigrateAgentLineagePanes(ctx context.Context, ownerID, hostID int64, server string, termToPane map[string]string, now int64) (bool, error) {
	if len(termToPane) == 0 {
		return false, nil
	}
	if now <= 0 {
		now = s.now().UnixMilli()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	rows, err := listAgentLineageQ(ctx, tx, ownerID, hostID, server)
	if err != nil {
		return false, err
	}
	changed := false
	for _, r := range rows {
		newChild := r.ChildPane
		if r.ChildTermID != "" {
			if p := strings.TrimSpace(termToPane[r.ChildTermID]); p != "" {
				newChild = p
			}
		}
		newParent := r.ParentPane
		if r.ParentTermID != "" {
			if p := strings.TrimSpace(termToPane[r.ParentTermID]); p != "" {
				newParent = p
			}
		}
		if newChild == r.ChildPane && newParent == r.ParentPane {
			continue
		}
		changed = true
		if newChild != r.ChildPane {
			// Primary key includes child_pane: replace row cleanly.
			if _, err := tx.ExecContext(ctx, `DELETE FROM agent_lineage
				WHERE owner_id = ? AND host_id = ? AND server = ? AND child_pane = ? AND child_term_id = ?`,
				ownerID, hostID, server, r.ChildPane, r.ChildTermID); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_lineage (
				owner_id, host_id, server, child_pane, child_term_id, child_session,
				parent_pane, parent_term_id, parent_session, source, state, evidence_key,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(owner_id, host_id, server, child_pane, child_term_id) DO UPDATE SET
				child_session = excluded.child_session,
				parent_pane = excluded.parent_pane,
				parent_term_id = excluded.parent_term_id,
				parent_session = excluded.parent_session,
				source = excluded.source,
				state = excluded.state,
				evidence_key = excluded.evidence_key,
				updated_at = excluded.updated_at`,
				r.OwnerID, r.HostID, r.Server, newChild, r.ChildTermID, r.ChildSession,
				newParent, r.ParentTermID, r.ParentSession, r.Source, r.State, r.EvidenceKey,
				r.CreatedAt, now); err != nil {
				return false, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_lineage
				SET parent_pane = ?, updated_at = ?
				WHERE owner_id = ? AND host_id = ? AND server = ? AND child_pane = ? AND child_term_id = ?`,
				newParent, now, ownerID, hostID, server, r.ChildPane, r.ChildTermID); err != nil {
				return false, err
			}
		}
	}
	if !changed {
		return false, nil
	}
	return true, tx.Commit()
}
