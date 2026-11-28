package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// CorraldPerfDailyDelta is a daily per-host/op/via rollup delta for successful
// operations.
type CorraldPerfDailyDelta struct {
	HostID  int64
	Day     string // UTC YYYY-MM-DD
	Op      string // "send", "card"
	Via     string // "ssh", "corrald"
	Count   int64
	TotalUs int64
}

// CorraldPerfTotal aggregates count and total duration in microseconds.
type CorraldPerfTotal struct {
	Count   int64
	TotalUs int64
}

// AddCorraldPerfDailyBatch atomically merges a batch of daily perf deltas into
// corrald_perf_daily. Deltas for hosts that were concurrently deleted are
// silently skipped so they never fail the rest of the batch.
func (s *Store) AddCorraldPerfDailyBatch(ctx context.Context, deltas []CorraldPerfDailyDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO corrald_perf_daily (host_id, day, op, via, count, total_us)
		SELECT ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM hosts WHERE id = ?)
		ON CONFLICT(host_id, day, op, via) DO UPDATE SET
			count = corrald_perf_daily.count + excluded.count,
			total_us = corrald_perf_daily.total_us + excluded.total_us`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range deltas {
		if d.HostID <= 0 || d.Day == "" || d.Op == "" || d.Via == "" || d.Count <= 0 {
			continue
		}
		if d.TotalUs < 0 {
			d.TotalUs = 0
		}
		if _, err := stmt.ExecContext(ctx, d.HostID, d.Day, d.Op, d.Via, d.Count, d.TotalUs, d.HostID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneCorraldPerfDaily deletes daily perf rows strictly older than cutoffDay
// (UTC YYYY-MM-DD).
func (s *Store) PruneCorraldPerfDaily(ctx context.Context, cutoffDay string) (int64, error) {
	if cutoffDay == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM corrald_perf_daily WHERE day < ?`, cutoffDay)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CorraldPerfDailyTotals sums count and total_us per op for hostID and via
// within [minDay, maxDay] (inclusive; empty bound is unbounded).
func (s *Store) CorraldPerfDailyTotals(ctx context.Context, hostID int64, via, minDay, maxDay string) (map[string]CorraldPerfTotal, error) {
	var conds []string
	args := []any{hostID, via}
	conds = append(conds, `host_id = ?`, `via = ?`)
	if minDay != "" {
		conds = append(conds, `day >= ?`)
		args = append(args, minDay)
	}
	if maxDay != "" {
		conds = append(conds, `day <= ?`)
		args = append(args, maxDay)
	}
	q := `SELECT op, SUM(count), SUM(total_us) FROM corrald_perf_daily WHERE ` + strings.Join(conds, " AND ") + ` GROUP BY op`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]CorraldPerfTotal)
	for rows.Next() {
		var op string
		var tot CorraldPerfTotal
		if err := rows.Scan(&op, &tot.Count, &tot.TotalUs); err != nil {
			return nil, err
		}
		out[op] = tot
	}
	return out, rows.Err()
}

// CorraldPerfDailyRows returns all stored daily perf rows for hostID (ordered
// by day, op, via).
func (s *Store) CorraldPerfDailyRows(ctx context.Context, hostID int64) ([]CorraldPerfDailyDelta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT host_id, day, op, via, count, total_us
		FROM corrald_perf_daily
		WHERE host_id = ?
		ORDER BY day ASC, op ASC, via ASC`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CorraldPerfDailyDelta
	for rows.Next() {
		var d CorraldPerfDailyDelta
		if err := rows.Scan(&d.HostID, &d.Day, &d.Op, &d.Via, &d.Count, &d.TotalUs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CorraldUpdateNotice records one host's update outcome (success or rolled-back
// failure) waiting to be shown to an enabled user.
type CorraldUpdateNotice struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"-"`
	HostID     int64  `json:"host_id"`
	HostName   string `json:"host_name"`
	OldVersion string `json:"old_version"`
	NewVersion string `json:"new_version"`
	NewRelease int64  `json:"new_release"`
	Success    bool   `json:"success"`
	CreatedAt  int64  `json:"created_at"`
	SeenAt     int64  `json:"-"`
}

// RecordCorraldUpdateNotices inserts pending update notices in a single
// transaction. Rows whose user or host no longer exists are skipped safely.
func (s *Store) RecordCorraldUpdateNotices(ctx context.Context, entries []CorraldUpdateNotice) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO corrald_update_notices (user_id, host_id, old_version, new_version, new_release, success, created_at, seen_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, 0
		WHERE EXISTS (SELECT 1 FROM users WHERE id = ?) AND EXISTS (SELECT 1 FROM hosts WHERE id = ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := s.unix()
	for _, e := range entries {
		if e.UserID <= 0 || e.HostID <= 0 {
			continue
		}
		ts := e.CreatedAt
		if ts <= 0 {
			ts = now
		}
		succ := 0
		if e.Success {
			succ = 1
		}
		if _, err := stmt.ExecContext(ctx, e.UserID, e.HostID, e.OldVersion, e.NewVersion, e.NewRelease, succ, ts, e.UserID, e.HostID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListUnseenCorraldUpdateNotices returns all unseen update notices for userID,
// joined with each host's display name.
func (s *Store) ListUnseenCorraldUpdateNotices(ctx context.Context, userID int64) ([]CorraldUpdateNotice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.user_id, n.host_id, COALESCE(h.name, ''), n.old_version, n.new_version, n.new_release, n.success, n.created_at, n.seen_at
		FROM corrald_update_notices n
		JOIN hosts h ON h.id = n.host_id
		WHERE n.user_id = ? AND n.seen_at = 0
		ORDER BY n.created_at ASC, n.id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CorraldUpdateNotice
	for rows.Next() {
		var n CorraldUpdateNotice
		var succ int
		if err := rows.Scan(&n.ID, &n.UserID, &n.HostID, &n.HostName, &n.OldVersion, &n.NewVersion, &n.NewRelease, &succ, &n.CreatedAt, &n.SeenAt); err != nil {
			return nil, err
		}
		n.Success = succ != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkCorraldUpdateNoticesSeen marks the specified notice IDs belonging to
// userID as seen.
func (s *Store) MarkCorraldUpdateNoticesSeen(ctx context.Context, userID int64, ids []int64) error {
	if userID <= 0 || len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, s.unix(), userID)
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(placeholders) == 0 {
		return nil
	}
	q := `UPDATE corrald_update_notices SET seen_at = ? WHERE user_id = ? AND seen_at = 0 AND id IN (` + strings.Join(placeholders, ",") + `)`
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// CorraldChangelogSeen tracks the last corrald version whose changelog a user viewed.
type CorraldChangelogSeen struct {
	LastVersion string
	LastRelease int64
	SeenAt      int64
}

// GetCorraldChangelogSeen returns the user's changelog cursor, or nil if never viewed.
func (s *Store) GetCorraldChangelogSeen(ctx context.Context, userID int64) (*CorraldChangelogSeen, error) {
	var c CorraldChangelogSeen
	err := s.db.QueryRowContext(ctx,
		`SELECT last_version, last_release, seen_at FROM corrald_changelog_seen WHERE user_id = ?`, userID).
		Scan(&c.LastVersion, &c.LastRelease, &c.SeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SetCorraldChangelogSeen records the latest version whose changelog userID viewed.
func (s *Store) SetCorraldChangelogSeen(ctx context.Context, userID int64, version string, release int64) error {
	if userID <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO corrald_changelog_seen (user_id, last_version, last_release, seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			last_version = excluded.last_version,
			last_release = excluded.last_release,
			seen_at = excluded.seen_at`,
		userID, version, release, s.unix())
	return err
}

// HostOwnerID returns the owner_id for hostID (internal gateway use).
func (s *Store) HostOwnerID(ctx context.Context, hostID int64) (int64, error) {
	var ownerID int64
	err := s.db.QueryRowContext(ctx, `SELECT owner_id FROM hosts WHERE id = ?`, hostID).Scan(&ownerID)
	if err != nil {
		return 0, notFound(err)
	}
	return ownerID, nil
}

// MarkCorraldHostsRemovePending transitions the given host IDs (and any non-off
// peers sharing fingerprint+username) to remove_pending so the host's files are
// cleaned up on next SSH connection or immediate cleanup.
func (s *Store) MarkCorraldHostsRemovePending(ctx context.Context, hostIDs []int64, fingerprint, username string) error {
	now := s.unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, id := range hostIDs {
		if id <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'remove_pending', active_op_id = '', updated_at = ?
			WHERE host_id = ? AND state NOT IN ('off')`, now, id); err != nil {
			return err
		}
	}
	if fingerprint != "" && username != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'remove_pending', active_op_id = '', updated_at = ?
			WHERE bound_fingerprint = ? AND bound_username = ? AND state NOT IN ('off')`, now, fingerprint, username); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkCorraldHostsStopped transitions active hosts for a disconnected account
// to stopped without downgrading hosts that are already in remove_pending or
// removing (transient check/cleanup failures must keep remove_pending for retry).
func (s *Store) MarkCorraldHostsStopped(ctx context.Context, hostIDs []int64, fingerprint, username string) error {
	now := s.unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, id := range hostIDs {
		if id <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'stopped', active_op_id = '', updated_at = ?
			WHERE host_id = ? AND state IN ('running', 'lost', 'updating', 'incompatible')`, now, id); err != nil {
			return err
		}
	}
	if fingerprint != "" && username != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'stopped', active_op_id = '', updated_at = ?
			WHERE bound_fingerprint = ? AND bound_username = ? AND state IN ('running', 'lost', 'updating', 'incompatible')`, now, fingerprint, username); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkCorraldHostsPreservedOtherGateway transitions active OR remove_pending /
// removing hosts to stopped ONLY when another gateway is confirmed to be using
// corrald (or an older daemon cannot support safe conditional shutdown/uninstall).
func (s *Store) MarkCorraldHostsPreservedOtherGateway(ctx context.Context, hostIDs []int64, fingerprint, username string) error {
	now := s.unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, id := range hostIDs {
		if id <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'stopped', active_op_id = '', updated_at = ?
			WHERE host_id = ? AND state IN ('running', 'lost', 'updating', 'incompatible', 'remove_pending', 'removing')`, now, id); err != nil {
			return err
		}
	}
	if fingerprint != "" && username != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'stopped', active_op_id = '', updated_at = ?
			WHERE bound_fingerprint = ? AND bound_username = ? AND state IN ('running', 'lost', 'updating', 'incompatible', 'remove_pending', 'removing')`, now, fingerprint, username); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CorraldHostWithOwner pairs a corrald_hosts row with its owning Corral user.
type CorraldHostWithOwner struct {
	HostID     int64
	OwnerID    int64
	CorralUser string
	Host       *CorraldHost
}

// ListCorraldHostsByStates returns all corrald_hosts rows whose state is in
// states, joined with hosts and users.
func (s *Store) ListCorraldHostsByStates(ctx context.Context, states ...string) ([]CorraldHostWithOwner, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := make([]string, 0, len(states))
	args := make([]any, 0, len(states))
	for _, st := range states {
		if st == "" {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, st)
	}
	if len(placeholders) == 0 {
		return nil, nil
	}
	q := `
		SELECT h.owner_id, COALESCE(u.username, ''), ` + corraldHostQualifiedCols + `
		FROM corrald_hosts ch
		JOIN hosts h ON h.id = ch.host_id
		LEFT JOIN users u ON u.id = h.owner_id
		WHERE ch.state IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY ch.host_id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CorraldHostWithOwner
	for rows.Next() {
		var ownerID int64
		var corralUser string
		ch := &CorraldHost{}
		var consentBy sql.NullInt64
		var filesJSON, pausedJSON, failedJSON string
		if err := rows.Scan(
			&ownerID, &corralUser,
			&ch.HostID, &ch.State, &consentBy, &ch.ConsentAt,
			&ch.BoundFingerprint, &ch.BoundUsername, &ch.BoundGateway,
			&ch.ManifestJSON, &ch.Version, &ch.Release,
			&ch.MachineID, &ch.SocketPath, &filesJSON, &pausedJSON, &failedJSON,
			&ch.ActiveOpID, &ch.UpdatedFromVer, &ch.UpdatedFromRel, &ch.UpdatedFromTarget,
			&ch.UpdateTargetVer, &ch.UpdateTargetRel, &ch.UpdateReadyAt, &ch.UpdateCrashCount,
			&ch.LastOKAt, &ch.LastError, &ch.RestartCount, &ch.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if consentBy.Valid {
			v := consentBy.Int64
			ch.ConsentBy = &v
		}
		if filesJSON != "" {
			_ = json.Unmarshal([]byte(filesJSON), &ch.Files)
		}
		if ch.Files == nil {
			ch.Files = []string{}
		}
		if pausedJSON != "" {
			_ = json.Unmarshal([]byte(pausedJSON), &ch.PausedUsers)
		}
		if ch.PausedUsers == nil {
			ch.PausedUsers = []int64{}
		}
		if failedJSON != "" {
			_ = json.Unmarshal([]byte(failedJSON), &ch.FailedReleases)
		}
		if ch.FailedReleases == nil {
			ch.FailedReleases = []int64{}
		}
		out = append(out, CorraldHostWithOwner{
			HostID:     ch.HostID,
			OwnerID:    ownerID,
			CorralUser: corralUser,
			Host:       ch,
		})
	}
	return out, rows.Err()
}

// HasCorraldHostsInStates reports whether any corrald_hosts row currently has
// one of the given states.
func (s *Store) HasCorraldHostsInStates(ctx context.Context, states ...string) (bool, error) {
	if len(states) == 0 {
		return false, nil
	}
	placeholders := make([]string, 0, len(states))
	args := make([]any, 0, len(states))
	for _, st := range states {
		if st == "" {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, st)
	}
	if len(placeholders) == 0 {
		return false, nil
	}
	var one int
	q := `SELECT 1 FROM corrald_hosts WHERE state IN (` + strings.Join(placeholders, ",") + `) LIMIT 1`
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ResetCorraldHostOff resets a single corrald_hosts row to 'off' after cleanup.
func (s *Store) ResetCorraldHostOff(ctx context.Context, hostID int64) error {
	if hostID <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = 'off', version = '', release = 0, socket_path = '', files_json = '[]',
		    paused_users_json = '[]', failed_releases_json = '[]', active_op_id = '',
		    updated_from_ver = '', updated_from_rel = 0, updated_from_target = '',
		    update_target_ver = '', update_target_rel = 0, update_ready_at = 0, update_crash_count = 0,
		    last_error = '', restart_count = 0, updated_at = ?
		WHERE host_id = ?`,
		s.unix(), hostID)
	return err
}
