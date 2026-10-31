package store

import (
	"context"
	"database/sql"
	"errors"
)

// Open tabs ------------------------------------------------------------------
//
// The tmux and herdr sessions a user has open on the gateway, kept so that
// they are opened again after it restarts (see web/restore.go). A record is
// written as its session is set up, before anyone can see or end it, and goes
// with it, except when the gateway stops.

// OpenTab is one such session.
type OpenTab struct {
	SessionID    string
	OwnerID      int64
	HostID       int64
	Kind         string // "tmux" or "herdr"
	Name         string // its tmux or herdr session; herdr's default is "default"
	HerdrDefault bool   // herdr's default session, as its discovery told it
	HostConfig   string // the host's settings it was opened with (web.hostConfig)
	Cols, Rows   int
	IdleSince    int64 // unix seconds since nobody of its owner looked at it; 0 while one does
	AliveAt      int64 // unix seconds it was last known to run
}

// LoginRule is what makes a login valid now (the auth service's policy, as
// cutoffs): seen since SeenSince, made since CreatedSince (unix seconds), its
// user not disabled, not told to choose a password, and the login not older
// than the user's password.
type LoginRule struct {
	SeenSince, CreatedSince int64
}

const validLogins = `FROM login_sessions ls JOIN users u ON u.id = ls.user_id
	WHERE ls.last_seen_at >= ? AND ls.created_at >= ? AND u.disabled = 0 AND u.must_change_password = 0 AND ls.created_at >= u.password_changed_at`

var (
	// ErrNoLogin: the login asked for (or, for a session opened again, every
	// login of its user) is not valid: nothing is kept.
	ErrNoLogin = errors.New("no valid login")
	// ErrHostChanged: the host's settings are not the ones asked for.
	ErrHostChanged = errors.New("the host's settings changed")
)

const tabCols = `session_id, owner_id, host_id, kind, name, herdr_default, host_config, cols, rows, idle_since, alive_at`

func scanTab(row interface{ Scan(...any) error }) (*OpenTab, error) {
	t := &OpenTab{}
	var def int
	if err := row.Scan(&t.SessionID, &t.OwnerID, &t.HostID, &t.Kind, &t.Name, &def, &t.HostConfig, &t.Cols, &t.Rows, &t.IdleSince, &t.AliveAt); err != nil {
		return nil, notFound(err)
	}
	t.HerdrDefault = def != 0
	return t, nil
}

func insertTab(ctx context.Context, tx *sql.Tx, t *OpenTab) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO open_tabs (`+tabCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.SessionID, t.OwnerID, t.HostID, t.Kind, t.Name, boolInt(t.HerdrDefault), t.HostConfig, t.Cols, t.Rows, t.IdleSince, t.AliveAt)
	return err
}

// RegisterTab keeps a session opened by a person, in one transaction: only
// while the login that opened it (loginHash) is valid and its user's (a
// revocation committed before finds none; one after removes it), taking
// the place of a record of the same session left from before (owner, host,
// kind, name and settings); and only while the host is its user's with
// the settings hostOK wants (ErrHostChanged). review523-2
func (s *Store) RegisterTab(ctx context.Context, t *OpenTab, loginHash string, r LoginRule, hostOK func(*Host) bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var uid int64
	err = tx.QueryRowContext(ctx, `SELECT ls.user_id `+validLogins+` AND ls.token_hash = ?`, r.SeenSince, r.CreatedSince, loginHash).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) || err == nil && uid != t.OwnerID {
		return ErrNoLogin
	}
	if err != nil {
		return err
	}
	if err := hostAsWanted(ctx, tx, t, hostOK); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM open_tabs WHERE owner_id = ? AND host_id = ? AND kind = ? AND name = ? AND host_config = ?`,
		t.OwnerID, t.HostID, t.Kind, t.Name, t.HostConfig); err != nil {
		return err
	}
	if err := insertTab(ctx, tx, t); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceTab keeps a session opened again in place of the record it was
// opened from (oldID), in one transaction: only while that record is still
// there (ErrNotFound: gone, revoked or taken), its user has a valid login
// (ErrNoLogin), and the host's settings are still what hostOK wants
// (ErrHostChanged; hostOK computes, it reads nothing).
func (s *Store) ReplaceTab(ctx context.Context, oldID string, t *OpenTab, r LoginRule, hostOK func(*Host) bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := affectedOrNotFound(tx.ExecContext(ctx, `DELETE FROM open_tabs WHERE session_id = ? AND owner_id = ?`, oldID, t.OwnerID)); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) `+validLogins+` AND ls.user_id = ?`, r.SeenSince, r.CreatedSince, t.OwnerID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNoLogin
	}
	if err := hostAsWanted(ctx, tx, t, hostOK); err != nil {
		return err
	}
	if err := insertTab(ctx, tx, t); err != nil {
		return err
	}
	return tx.Commit()
}

// hostAsWanted: the record's host is its user's, as hostOK wants it (read
// in tx).
func hostAsWanted(ctx context.Context, tx *sql.Tx, t *OpenTab, hostOK func(*Host) bool) error {
	h, err := scanHost(tx.QueryRowContext(ctx, `SELECT `+hostCols+` FROM hosts WHERE owner_id = ? AND id = ?`, t.OwnerID, t.HostID))
	if errors.Is(err, ErrNotFound) {
		return ErrHostChanged
	}
	if err != nil {
		return err
	}
	if !hostOK(h) {
		return ErrHostChanged
	}
	return nil
}

// HasLogin: the user has a valid login now (by r), as RegisterTab tells it.
func (s *Store) HasLogin(ctx context.Context, userID int64, r LoginRule) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) `+validLogins+` AND ls.user_id = ?`, r.SeenSince, r.CreatedSince, userID).Scan(&n)
	return n > 0, err
}

// OpenTabByID is one record, of its owner.
func (s *Store) OpenTabByID(ctx context.Context, ownerID int64, id string) (*OpenTab, error) {
	return scanTab(s.db.QueryRowContext(ctx, `SELECT `+tabCols+` FROM open_tabs WHERE session_id = ? AND owner_id = ?`, id, ownerID))
}

// OpenTabs is every record, the newest known to run first, at most limit.
func (s *Store) OpenTabs(ctx context.Context, limit int) ([]*OpenTab, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tabCols+` FROM open_tabs ORDER BY alive_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OpenTab
	for rows.Next() {
		t, err := scanTab(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTab removes the record of one session (of its owner).
func (s *Store) DeleteTab(ctx context.Context, ownerID int64, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM open_tabs WHERE session_id = ? AND owner_id = ?`, id, ownerID)
	return err
}

// ClearTabs removes every record (opening again turned off).
func (s *Store) ClearTabs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM open_tabs`)
	return err
}

// TabState is what is known of a running session now, to keep.
type TabState struct {
	SessionID  string
	OwnerID    int64
	AliveAt    int64
	IdleSince  int64
	Cols, Rows int
}

// KeepTabs records sessions as still running (alive_at, idle_since, size),
// by their ids only, a hundred to a transaction.
func (s *Store) KeepTabs(ctx context.Context, states []TabState) error {
	for len(states) > 0 {
		n := min(len(states), 100)
		if err := s.keepTabs(ctx, states[:n]); err != nil {
			return err
		}
		states = states[n:]
	}
	return nil
}

func (s *Store) keepTabs(ctx context.Context, states []TabState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, st := range states {
		if _, err := tx.ExecContext(ctx, `UPDATE open_tabs SET alive_at = ?, idle_since = ?, cols = ?, rows = ? WHERE session_id = ? AND owner_id = ?`,
			st.AliveAt, st.IdleSince, st.Cols, st.Rows, st.SessionID, st.OwnerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func deleteUserTabs(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM open_tabs WHERE owner_id = ?`, userID)
	return err
}

// EndAccessTx removes every login of the user and every record of their
// sessions, in one transaction (access ended: nothing opened again for them).
func (s *Store) EndAccessTx(ctx context.Context, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if err := deleteUserTabs(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// LogoutTx removes a login and, where its user has no other valid one left
// (by r), every record of their sessions, in one transaction (the sessions
// themselves are not closed: other devices may use them).
func (s *Store) LogoutTx(ctx context.Context, tokenHash string, r LoginRule) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var uid int64
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM login_sessions WHERE token_hash = ?`, tokenHash).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) `+validLogins+` AND ls.user_id = ?`, r.SeenSince, r.CreatedSince, uid).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if err := deleteUserTabs(ctx, tx, uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}
