package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

type User struct {
	ID                int64  `json:"id"`
	Username          string `json:"username"`
	DisplayName       string `json:"display_name"`
	Role              string `json:"role"`
	Disabled          bool   `json:"disabled"`
	CreatedAt         int64  `json:"created_at"`
	PasswordChangedAt int64  `json:"password_changed_at"`

	passwordHash string
	failedLogins int
	lockedUntil  int64
}

func (u *User) PasswordHash() string { return u.passwordHash }
func (u *User) FailedLogins() int    { return u.failedLogins }
func (u *User) LockedUntil() int64   { return u.lockedUntil }
func (u *User) IsAdmin() bool        { return u.Role == RoleAdmin }

var ErrUsernameTaken = errors.New("username taken")

const userCols = `id, username, display_name, password_hash, role, disabled, failed_logins, locked_until, created_at, password_changed_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var disabled int
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.passwordHash, &u.Role, &disabled,
		&u.failedLogins, &u.lockedUntil, &u.CreatedAt, &u.PasswordChangedAt)
	if err != nil {
		return nil, notFound(err)
	}
	u.Disabled = disabled != 0
	return u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash, role string) (*User, error) {
	now := s.unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, display_name, password_hash, role, created_at, password_changed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		username, displayName, passwordHash, role, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(ctx, id)
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ? AND disabled = 0`, RoleAdmin).Scan(&n)
	return n, err
}

// RecordLoginFailure increments the failure count atomically and returns the
// new count, so concurrent wrong guesses cannot each think they were the first.
func (s *Store) RecordLoginFailure(ctx context.Context, id int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `UPDATE users SET failed_logins = failed_logins + 1 WHERE id = ? RETURNING failed_logins`, id).Scan(&n)
	return n, err
}

// ExtendLockout pushes the lockout forward, never backward.
func (s *Store) ExtendLockout(ctx context.Context, id, until int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET locked_until = MAX(locked_until, ?) WHERE id = ?`, until, id)
	return err
}

func (s *Store) ResetLoginFailures(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET failed_logins = 0, locked_until = 0 WHERE id = ?`, id)
	return err
}

func (s *Store) SetPassword(ctx context.Context, id int64, hash string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_changed_at = ?, failed_logins = 0, locked_until = 0 WHERE id = ?`, hash, s.unix(), id))
}

// ChangePasswordIf replaces the password only if it is still the one the
// caller verified and the caller's login still exists, and signs out every
// other login in the same transaction. A slow change request can therefore
// not undo an administrator's reset that happened meanwhile.
func (s *Store) ChangePasswordIf(ctx context.Context, userID int64, verifiedHash, newHash, keepSession string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_sessions WHERE token_hash = ? AND user_id = ?`, keepSession, userID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := affectedOrNotFound(tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_changed_at = ?, failed_logins = 0, locked_until = 0
		 WHERE id = ? AND password_hash = ? AND disabled = 0`, newHash, s.unix(), userID, verifiedHash)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_sessions WHERE user_id = ? AND token_hash != ?`, userID, keepSession); err != nil {
		return err
	}
	// The browser that changed the password stays signed in; its login counts
	// as created now so it passes the "older than the password" check.
	if _, err := tx.ExecContext(ctx, `UPDATE login_sessions SET created_at = ? WHERE token_hash = ?`, s.unix(), keepSession); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetPasswordTx sets a new password and removes every login of the user in
// one transaction (the admin reset path).
func (s *Store) ResetPasswordTx(ctx context.Context, userID int64, hash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := affectedOrNotFound(tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_changed_at = ?, failed_logins = 0, locked_until = 0 WHERE id = ?`, hash, s.unix(), userID)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrLastAdmin = errors.New("at least one active administrator must remain")

// UpdateUserGuarded changes role and/or disabled state in one transaction and
// refuses a change that would leave no active administrator.
func (s *Store) UpdateUserGuarded(ctx context.Context, id int64, role *string, disabled *bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role != nil {
		if err := affectedOrNotFound(tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, *role, id)); err != nil {
			return err
		}
	}
	if disabled != nil {
		v := 0
		if *disabled {
			v = 1
		}
		if err := affectedOrNotFound(tx.ExecContext(ctx, `UPDATE users SET disabled = ? WHERE id = ?`, v, id)); err != nil {
			return err
		}
		if *disabled {
			if _, err := tx.ExecContext(ctx, `DELETE FROM login_sessions WHERE user_id = ?`, id); err != nil {
				return err
			}
		}
	}
	var admins int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ? AND disabled = 0`, RoleAdmin).Scan(&admins); err != nil {
		return err
	}
	if admins == 0 {
		return ErrLastAdmin
	}
	return tx.Commit()
}

func (s *Store) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	return affectedOrNotFound(s.db.ExecContext(ctx, `UPDATE users SET disabled = ? WHERE id = ?`, v, id))
}

func (s *Store) SetRole(ctx context.Context, id int64, role string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id))
}

func (s *Store) SetDisplayName(ctx context.Context, id int64, name string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `UPDATE users SET display_name = ? WHERE id = ?`, name, id))
}

// Login sessions -----------------------------------------------------------

type LoginSession struct {
	TokenHash  string
	UserID     int64
	CreatedAt  int64
	LastSeenAt int64
	IP         string
	UserAgent  string
}

func (s *Store) CreateLoginSession(ctx context.Context, tokenHash string, userID int64, ip, ua string) error {
	now := s.unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO login_sessions (token_hash, user_id, created_at, last_seen_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, now, now, ip, ua)
	return err
}

// CreateLoginSessionIfCurrent creates a login only if the account still has the
// password hash we verified and is not disabled. A sign-in that started before
// a password change or a disable therefore cannot complete after it.
func (s *Store) CreateLoginSessionIfCurrent(ctx context.Context, tokenHash string, userID int64, verifiedHash, ip, ua string) error {
	now := s.unix()
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`INSERT INTO login_sessions (token_hash, user_id, created_at, last_seen_at, ip, user_agent)
		 SELECT ?, id, ?, ?, ?, ? FROM users WHERE id = ? AND password_hash = ? AND disabled = 0`,
		tokenHash, now, now, ip, ua, userID, verifiedHash))
}

func (s *Store) LoginSessionByHash(ctx context.Context, tokenHash string) (*LoginSession, error) {
	ls := &LoginSession{}
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, last_seen_at, ip, user_agent FROM login_sessions WHERE token_hash = ?`, tokenHash).
		Scan(&ls.TokenHash, &ls.UserID, &ls.CreatedAt, &ls.LastSeenAt, &ls.IP, &ls.UserAgent)
	if err != nil {
		return nil, notFound(err)
	}
	return ls, nil
}

func (s *Store) TouchLoginSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE login_sessions SET last_seen_at = ? WHERE token_hash = ?`, s.unix(), tokenHash)
	return err
}

func (s *Store) DeleteLoginSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserLoginSessions removes all of a user's logins, optionally keeping one.
func (s *Store) DeleteUserLoginSessions(ctx context.Context, userID int64, keepHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_sessions WHERE user_id = ? AND token_hash != ?`, userID, keepHash)
	return err
}

func (s *Store) PruneLoginSessions(ctx context.Context, idleBefore, createdBefore int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_sessions WHERE last_seen_at < ? OR created_at < ?`, idleBefore, createdBefore)
	return err
}

// Invites ------------------------------------------------------------------

type Invite struct {
	TokenHash string `json:"-"`
	Role      string `json:"role"`
	Note      string `json:"note"`
	CreatedBy int64  `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	UsedAt    int64  `json:"used_at"`
}

func (s *Store) CreateInvite(ctx context.Context, tokenHash, role, note string, createdBy, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invites (token_hash, role, note, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		tokenHash, role, note, createdBy, s.unix(), expiresAt)
	return err
}

func (s *Store) InviteByHash(ctx context.Context, tokenHash string) (*Invite, error) {
	inv := &Invite{TokenHash: tokenHash}
	err := s.db.QueryRowContext(ctx,
		`SELECT role, note, created_by, created_at, expires_at, used_at FROM invites WHERE token_hash = ?`, tokenHash).
		Scan(&inv.Role, &inv.Note, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.UsedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return inv, nil
}

// RedeemInvite atomically marks an invite used and creates the user.
func (s *Store) RedeemInvite(ctx context.Context, tokenHash, username, displayName, passwordHash string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.unix()
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM invites WHERE token_hash = ? AND used_at = 0 AND expires_at > ?`, tokenHash, now).Scan(&role)
	if err != nil {
		return nil, notFound(err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, display_name, password_hash, role, created_at, password_changed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		username, displayName, passwordHash, role, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET used_at = ?, used_by = ? WHERE token_hash = ?`, now, id, tokenHash); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.UserByID(ctx, id)
}

// Settings -----------------------------------------------------------------

func (s *Store) UserSettings(ctx context.Context, userID int64) (string, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM user_settings WHERE user_id = ?`, userID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "{}", nil
	}
	return data, err
}

func (s *Store) SaveUserSettings(ctx context.Context, userID int64, data string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_settings (user_id, data) VALUES (?, ?) ON CONFLICT(user_id) DO UPDATE SET data = excluded.data`, userID, data)
	return err
}
