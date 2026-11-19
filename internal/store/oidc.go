package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// OIDCIdentity is an account's binding to an identity of a single sign-on
// provider: its issuer and its subject for the person, and what it said of
// them last (for showing only: never for telling who someone is).
type OIDCIdentity struct {
	Issuer       string `json:"issuer"`
	Subject      string `json:"subject"`
	UserID       int64  `json:"user_id"`
	UsernameHint string `json:"username_hint"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	CreatedAt    int64  `json:"created_at"`
	LastLoginAt  int64  `json:"last_login_at"`
}

var (
	// ErrIdentityTaken: the identity is bound to another account.
	ErrIdentityTaken = errors.New("this single sign-on identity belongs to another account")
	// ErrAlreadyLinked: the account is bound to an identity of that provider already.
	ErrAlreadyLinked = errors.New("this account is already linked to that provider")
	// ErrNoPassword: the account has no password, single sign-on being its only way in.
	ErrNoPassword = errors.New("this account has no password")
)

const identityCols = `issuer, subject, user_id, username_hint, email, name, created_at, last_login_at`

func scanIdentity(row interface{ Scan(...any) error }) (*OIDCIdentity, error) {
	i := &OIDCIdentity{}
	if err := row.Scan(&i.Issuer, &i.Subject, &i.UserID, &i.UsernameHint, &i.Email, &i.Name, &i.CreatedAt, &i.LastLoginAt); err != nil {
		return nil, notFound(err)
	}
	return i, nil
}

// IdentityBySubject is the binding of the provider's (issuer's) subject, or ErrNotFound.
func (s *Store) IdentityBySubject(ctx context.Context, issuer, subject string) (*OIDCIdentity, error) {
	return scanIdentity(s.db.QueryRowContext(ctx, `SELECT `+identityCols+` FROM oidc_identities WHERE issuer = ? AND subject = ?`, issuer, subject))
}

// IdentitiesOf: an account's bindings.
func (s *Store) IdentitiesOf(ctx context.Context, userID int64) ([]*OIDCIdentity, error) {
	return s.identities(ctx, `SELECT `+identityCols+` FROM oidc_identities WHERE user_id = ? ORDER BY issuer`, userID)
}

// AllIdentities: every account's bindings (the administrators' list of users).
func (s *Store) AllIdentities(ctx context.Context) ([]*OIDCIdentity, error) {
	return s.identities(ctx, `SELECT `+identityCols+` FROM oidc_identities ORDER BY user_id, issuer`)
}

func (s *Store) identities(ctx context.Context, q string, args ...any) ([]*OIDCIdentity, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OIDCIdentity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// CreateSSOUser makes an account for an identity signing in the first time
// (no password: single sign-on its way in) and binds it, in one
// transaction. Its user name is base, or base-2, base-3 … — the first no
// account has: never one taken, an existing account's whoever it is.
// ErrIdentityTaken where the identity was bound meanwhile.
func (s *Store) CreateSSOUser(ctx context.Context, id *OIDCIdentity, base, displayName, role string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.unix()
	var uid int64
	for n := 1; n <= 100 && uid == 0; n++ {
		name := base
		if n > 1 {
			suffix := "-" + strconv.Itoa(n)
			name = trimTo(base, 32-len(suffix)) + suffix
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO users (username, display_name, password_hash, role, created_at, password_changed_at, must_change_password) VALUES (?, ?, '', ?, ?, ?, 0)`,
			name, displayName, role, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				continue
			}
			return nil, err
		}
		uid, _ = res.LastInsertId()
	}
	if uid == 0 {
		return nil, ErrUsernameTaken
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO oidc_identities (`+identityCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Issuer, id.Subject, uid, id.UsernameHint, id.Email, id.Name, now, now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrIdentityTaken
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.UserByID(ctx, uid)
}

// trimTo cuts s to at most n bytes, not inside a character.
func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// LinkIdentity binds an identity to an account that asked for it (signed
// in, and signed in at the provider as it). ErrIdentityTaken: another
// account's; ErrAlreadyLinked: this one has an identity of that provider.
func (s *Store) LinkIdentity(ctx context.Context, userID int64, id *OIDCIdentity) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner int64
	switch err := tx.QueryRowContext(ctx, `SELECT user_id FROM oidc_identities WHERE issuer = ? AND subject = ?`, id.Issuer, id.Subject).Scan(&owner); {
	case err == nil && owner == userID:
		return nil // (already this one's)
	case err == nil:
		return ErrIdentityTaken
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oidc_identities WHERE user_id = ? AND issuer = ?`, userID, id.Issuer).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrAlreadyLinked
	}
	now := s.unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO oidc_identities (`+identityCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Issuer, id.Subject, userID, id.UsernameHint, id.Email, id.Name, now, now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrIdentityTaken
		}
		return err
	}
	return tx.Commit()
}

// UnlinkIdentity removes an account's binding to the provider (issuer), and
// ends the logins made by it. ErrNoPassword where the account has no
// password (it would have no way in left); ErrNotFound where it had none.
// It gives back the logins ended.
func (s *Store) UnlinkIdentity(ctx context.Context, userID int64, issuer string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var hash string
	if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash); err != nil {
		return nil, notFound(err)
	}
	if hash == "" {
		return nil, ErrNoPassword
	}
	var subject string
	if err := tx.QueryRowContext(ctx, `SELECT subject FROM oidc_identities WHERE user_id = ? AND issuer = ?`, userID, issuer).Scan(&subject); err != nil {
		return nil, notFound(err)
	}
	ended, err := endOIDCLogins(ctx, tx, issuer, subject, "")
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_identities WHERE user_id = ? AND issuer = ?`, userID, issuer); err != nil {
		return nil, err
	}
	return ended, tx.Commit()
}

// TouchIdentity notes a sign-in by the identity, and what the provider said
// of the person this time.
func (s *Store) TouchIdentity(ctx context.Context, id *OIDCIdentity) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oidc_identities SET username_hint = ?, email = ?, name = ?, last_login_at = ? WHERE issuer = ? AND subject = ?`,
		id.UsernameHint, id.Email, id.Name, s.unix(), id.Issuer, id.Subject)
	return err
}

// RecordOIDCLogin notes that a login was made by single sign-on as the
// identity, in the provider's session sid ("" where it gave none).
func (s *Store) RecordOIDCLogin(ctx context.Context, tokenHash, issuer, subject, sid string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_logins (token_hash, issuer, subject, sid) VALUES (?, ?, ?, ?)`, tokenHash, issuer, subject, sid)
	return err
}

// OIDCLoginOf: the identity a login was made by single sign-on as (ok false:
// not by single sign-on).
func (s *Store) OIDCLoginOf(ctx context.Context, tokenHash string) (issuer, subject, sid string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT issuer, subject, sid FROM oidc_logins WHERE token_hash = ?`, tokenHash).Scan(&issuer, &subject, &sid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	return issuer, subject, sid, err == nil, err
}

// EndOIDCLogins ends the logins single sign-on made — for back-channel
// logout: those of the provider's session sid where one is given (and, with
// it, of subject where that is given too), else all of subject's. Logins
// made with a password are not among them. It gives back the logins ended,
// with their accounts.
func (s *Store) EndOIDCLogins(ctx context.Context, issuer, subject, sid string) (map[string]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := `SELECT l.token_hash, s.user_id FROM oidc_logins l JOIN login_sessions s ON s.token_hash = l.token_hash WHERE l.issuer = ?`
	args := []any{issuer}
	if sid != "" {
		q += ` AND l.sid = ?`
		args = append(args, sid)
	}
	if subject != "" {
		q += ` AND l.subject = ?`
		args = append(args, subject)
	}
	if sid == "" && subject == "" {
		return nil, errors.New("neither a subject nor a session")
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for rows.Next() {
		var h string
		var uid int64
		if err := rows.Scan(&h, &uid); err != nil {
			rows.Close()
			return nil, err
		}
		out[h] = uid
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for h := range out {
		if _, err := tx.ExecContext(ctx, `DELETE FROM login_sessions WHERE token_hash = ?`, h); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// endOIDCLogins: EndOIDCLogins of subject in a transaction of the caller's.
func endOIDCLogins(ctx context.Context, tx *sql.Tx, issuer, subject, sid string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT token_hash FROM oidc_logins WHERE issuer = ? AND subject = ?`, issuer, subject)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, h)
	}
	rows.Close()
	for _, h := range out {
		if _, err := tx.ExecContext(ctx, `DELETE FROM login_sessions WHERE token_hash = ?`, h); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

// HasPassword: the account has a password (not single sign-on alone).
func (u *User) HasPassword() bool { return u.passwordHash != "" }
