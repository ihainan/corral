package store

import (
	"context"
	"database/sql"
	"errors"
)

// CorraldHostHint records whether a contextual corrald enablement hint has
// already been shown to userID for hostID or explicitly dismissed by the user.
type CorraldHostHint struct {
	UserID      int64
	HostID      int64
	ShownAt     int64
	DismissedAt int64
}

// CorraldHostHint returns the hint tracking row for (userID, hostID), or
// ErrNotFound if none has been recorded.
func (s *Store) CorraldHostHint(ctx context.Context, userID, hostID int64) (*CorraldHostHint, error) {
	var h CorraldHostHint
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, host_id, shown_at, dismissed_at FROM corrald_host_hints WHERE user_id = ? AND host_id = ?`,
		userID, hostID,
	).Scan(&h.UserID, &h.HostID, &h.ShownAt, &h.DismissedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &h, nil
}

// ListCorraldHostHints returns all hint tracking rows for userID keyed by host_id.
func (s *Store) ListCorraldHostHints(ctx context.Context, userID int64) (map[int64]CorraldHostHint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, host_id, shown_at, dismissed_at FROM corrald_host_hints WHERE user_id = ?`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]CorraldHostHint)
	for rows.Next() {
		var h CorraldHostHint
		if err := rows.Scan(&h.UserID, &h.HostID, &h.ShownAt, &h.DismissedAt); err != nil {
			return nil, err
		}
		out[h.HostID] = h
	}
	return out, rows.Err()
}

// MarkCorraldHintShown persists that the hint banner was displayed to userID
// for hostID. Preserves an earlier shown_at timestamp and any dismissed_at.
// Returns ErrNotFound if hostID does not exist or is not owned by userID.
func (s *Store) MarkCorraldHintShown(ctx context.Context, userID, hostID int64) error {
	if userID <= 0 || hostID <= 0 {
		return ErrNotFound
	}
	now := s.unix()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO corrald_host_hints (user_id, host_id, shown_at, dismissed_at)
		SELECT ?, ?, ?, 0
		WHERE EXISTS (SELECT 1 FROM users WHERE id = ?)
		  AND EXISTS (SELECT 1 FROM hosts WHERE id = ? AND owner_id = ?)
		ON CONFLICT(user_id, host_id) DO UPDATE SET
			shown_at = CASE WHEN corrald_host_hints.shown_at = 0 THEN excluded.shown_at ELSE corrald_host_hints.shown_at END`,
		userID, hostID, now, userID, hostID, userID,
	)
	return affectedOrNotFound(res, err)
}

// MarkCorraldHintDismissed persists that userID clicked "don't prompt again"
// for hostID (and also ensures shown_at is set). Returns ErrNotFound if hostID
// does not exist or is not owned by userID.
func (s *Store) MarkCorraldHintDismissed(ctx context.Context, userID, hostID int64) error {
	if userID <= 0 || hostID <= 0 {
		return ErrNotFound
	}
	now := s.unix()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO corrald_host_hints (user_id, host_id, shown_at, dismissed_at)
		SELECT ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM users WHERE id = ?)
		  AND EXISTS (SELECT 1 FROM hosts WHERE id = ? AND owner_id = ?)
		ON CONFLICT(user_id, host_id) DO UPDATE SET
			shown_at = CASE WHEN corrald_host_hints.shown_at = 0 THEN excluded.shown_at ELSE corrald_host_hints.shown_at END,
			dismissed_at = CASE WHEN corrald_host_hints.dismissed_at = 0 THEN excluded.dismissed_at ELSE corrald_host_hints.dismissed_at END`,
		userID, hostID, now, now, userID, hostID, userID,
	)
	return affectedOrNotFound(res, err)
}

// ResetCorraldHintsForUser clears all per-host shown/dismissed records for
// userID so previously dismissed or shown hosts become eligible again when the
// user re-enables the account-level hint switch.
func (s *Store) ResetCorraldHintsForUser(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM corrald_host_hints WHERE user_id = ?`, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
