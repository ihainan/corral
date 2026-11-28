package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// CorraldHintsResetKey is the server-managed key inside user_settings.data
// storing the monotonic millisecond timestamp of the user's most recent
// corraldHints false->true reset. It is intentionally NOT in the client
// settings whitelist.
const CorraldHintsResetKey = "corraldHintsResetAt"

// ErrInvalidHintGeneration is returned when a client supplies a negative or
// future hint generation value.
var ErrInvalidHintGeneration = errors.New("invalid corrald hint generation")

// CorraldHostHint records whether a contextual corrald enablement hint has
// been shown to userID for hostID or explicitly dismissed by the user
// (timestamps in Unix milliseconds).
type CorraldHostHint struct {
	UserID      int64
	HostID      int64
	ShownAt     int64
	DismissedAt int64
}

// HintTimestampMillis normalizes a stored hint timestamp to Unix milliseconds
// (preserving millisecond timestamps and upgrading any pre-existing second
// timestamps < 1e12).
func HintTimestampMillis(ts int64) int64 {
	if ts > 0 && ts < 1_000_000_000_000 {
		return ts * 1000
	}
	return ts
}

// ParseCorraldHintsSettings extracts the user's corraldHints switch (default
// true when absent) and server-written corraldHintsResetAt generation (ms,
// default 0 when absent) from raw user_settings JSON.
func ParseCorraldHintsSettings(raw string) (enabled bool, resetAt int64) {
	enabled = true
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &obj) != nil || obj == nil {
		return true, 0
	}
	if v, ok := obj["corraldHints"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			enabled = b
		}
	}
	if v, ok := obj[CorraldHintsResetKey]; ok {
		var ts int64
		if json.Unmarshal(v, &ts) == nil && ts > 0 {
			resetAt = ts
		}
	}
	return enabled, resetAt
}

// CorraldHintRowActive reports whether row counts as shown or dismissed under
// the user's current resetAt generation (milliseconds).
func CorraldHintRowActive(row *CorraldHostHint, resetAt int64) bool {
	if row == nil {
		return false
	}
	shown := HintTimestampMillis(row.ShownAt)
	dismissed := HintTimestampMillis(row.DismissedAt)
	if resetAt <= 0 {
		return shown > 0 || dismissed > 0
	}
	return shown >= resetAt || dismissed >= resetAt
}

// MergeUserSettingsWithHintMax reads a user's settings JSON and the maximum
// existing corrald_host_hints timestamp (in milliseconds) in a single SQLite
// transaction, invokes merge, and writes the updated JSON back atomically.
func (s *Store) MergeUserSettingsWithHintMax(ctx context.Context, userID int64, merge func(old string, maxHintTS int64) (string, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	old := "{}"
	err = tx.QueryRowContext(ctx, `SELECT data FROM user_settings WHERE user_id = ?`, userID).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var rawMaxShown, rawMaxDismissed int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(shown_at), 0), COALESCE(MAX(dismissed_at), 0) FROM corrald_host_hints WHERE user_id = ?`,
		userID,
	).Scan(&rawMaxShown, &rawMaxDismissed); err != nil {
		return err
	}
	maxHintTS := max(HintTimestampMillis(rawMaxShown), HintTimestampMillis(rawMaxDismissed))

	data, err := merge(old, maxHintTS)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_settings (user_id, data) VALUES (?, ?) ON CONFLICT(user_id) DO UPDATE SET data = excluded.data`, userID, data); err != nil {
		return err
	}
	return tx.Commit()
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

// ClaimCorraldHintShown atomically claims the one-time display slot for
// (userID, hostID) under generation. It writes shown_at using a single
// conditional INSERT ... ON CONFLICT DO UPDATE ... WHERE statement ONLY when:
//   - hostID exists and is owned by userID (otherwise ErrNotFound)
//   - user's corraldHints master switch is enabled
//   - generation matches the user's current corraldHintsResetAt (stale returns
//     false, nil; negative or future returns ErrInvalidHintGeneration)
//   - neither shown_at nor dismissed_at is already active for this generation
//
// Two concurrent callers racing for the same (userID, hostID, generation) are
// guaranteed to receive claimed=true at most once.
func (s *Store) ClaimCorraldHintShown(ctx context.Context, userID, hostID, generation int64) (bool, error) {
	if userID <= 0 || hostID <= 0 {
		return false, ErrNotFound
	}
	if generation < 0 {
		return false, ErrInvalidHintGeneration
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts WHERE id = ? AND owner_id = ?`, hostID, userID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, ErrNotFound
	}

	raw := "{}"
	err = tx.QueryRowContext(ctx, `SELECT data FROM user_settings WHERE user_id = ?`, userID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	enabled, currentResetAt := ParseCorraldHintsSettings(raw)

	if !enabled || generation < currentResetAt {
		return false, nil
	}
	if generation > currentResetAt {
		return false, ErrInvalidHintGeneration
	}

	now := s.unixMilli()
	if now < currentResetAt {
		now = currentResetAt
	}
	if now <= 0 {
		now = 1
	}
	minActive := max(currentResetAt, 1)

	res, err := tx.ExecContext(ctx, `
		INSERT INTO corrald_host_hints (user_id, host_id, shown_at, dismissed_at)
		VALUES (?, ?, ?, 0)
		ON CONFLICT(user_id, host_id) DO UPDATE SET
			shown_at = excluded.shown_at
		WHERE
			(CASE WHEN corrald_host_hints.shown_at > 0 AND corrald_host_hints.shown_at < 1000000000000 THEN corrald_host_hints.shown_at * 1000 ELSE corrald_host_hints.shown_at END) < ?
			AND
			(CASE WHEN corrald_host_hints.dismissed_at > 0 AND corrald_host_hints.dismissed_at < 1000000000000 THEN corrald_host_hints.dismissed_at * 1000 ELSE corrald_host_hints.dismissed_at END) < ?`,
		userID, hostID, now, minActive, minActive,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkCorraldHintDismissed atomically checks the user's current reset
// generation and persists that userID clicked "don't prompt again" for hostID
// (also recording shown_at for the current generation).
//   - Returns (false, nil) with no DB write if generation is older than the
//     user's current corraldHintsResetAt.
//   - Returns (false, ErrInvalidHintGeneration) if generation is negative or
//     greater than the user's current reset generation.
//   - Returns (false, ErrNotFound) if hostID does not exist or is not owned by userID.
func (s *Store) MarkCorraldHintDismissed(ctx context.Context, userID, hostID, generation int64) (bool, error) {
	if userID <= 0 || hostID <= 0 {
		return false, ErrNotFound
	}
	if generation < 0 {
		return false, ErrInvalidHintGeneration
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts WHERE id = ? AND owner_id = ?`, hostID, userID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, ErrNotFound
	}

	raw := "{}"
	err = tx.QueryRowContext(ctx, `SELECT data FROM user_settings WHERE user_id = ?`, userID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, currentResetAt := ParseCorraldHintsSettings(raw)

	if generation < currentResetAt {
		return false, nil
	}
	if generation > currentResetAt {
		return false, ErrInvalidHintGeneration
	}

	now := s.unixMilli()
	if now < currentResetAt {
		now = currentResetAt
	}
	if now <= 0 {
		now = 1
	}
	minActive := max(currentResetAt, 1)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO corrald_host_hints (user_id, host_id, shown_at, dismissed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, host_id) DO UPDATE SET
			shown_at = CASE
				WHEN (CASE WHEN corrald_host_hints.shown_at > 0 AND corrald_host_hints.shown_at < 1000000000000 THEN corrald_host_hints.shown_at * 1000 ELSE corrald_host_hints.shown_at END) < ?
				THEN excluded.shown_at
				ELSE corrald_host_hints.shown_at
			END,
			dismissed_at = CASE
				WHEN (CASE WHEN corrald_host_hints.dismissed_at > 0 AND corrald_host_hints.dismissed_at < 1000000000000 THEN corrald_host_hints.dismissed_at * 1000 ELSE corrald_host_hints.dismissed_at END) < ?
				THEN excluded.dismissed_at
				ELSE corrald_host_hints.dismissed_at
			END`,
		userID, hostID, now, now, minActive, minActive,
	)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
