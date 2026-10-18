package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
)

// App settings -----------------------------------------------------------

func (s *Store) AppSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetAppSettingOnce stores a value unless one exists, and returns the stored value.
func (s *Store) SetAppSettingOnce(ctx context.Context, key, value string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO app_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, key, value); err != nil {
		return "", err
	}
	v, _, err := s.AppSetting(ctx, key)
	return v, err
}

// Push subscriptions -----------------------------------------------------

type PushSubscription struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"-"`
	LoginHash string `json:"-"` // the browser login it belongs to; deleting that login deletes it
	Endpoint  string `json:"-"`
	P256dh    string `json:"-"`
	Auth      string `json:"-"`
	UserAgent string `json:"user_agent"`
	CreatedAt int64  `json:"created_at"`
	LastOKAt  int64  `json:"last_ok_at"`
}

// SavePushSubscription upserts by endpoint. An endpoint moving to another user
// or login (shared device, new sign-in) is reassigned rather than duplicated.
// The login must still exist (ErrNotFound otherwise). Beyond keep
// subscriptions per user, the oldest are dropped.
func (s *Store) SavePushSubscription(ctx context.Context, p *PushSubscription, keep int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := affectedOrNotFound(tx.ExecContext(ctx,
		`INSERT INTO push_subscriptions (user_id, login_hash, endpoint, p256dh, auth, user_agent, created_at)
		 SELECT user_id, token_hash, ?, ?, ?, ?, ? FROM login_sessions WHERE token_hash = ? AND user_id = ?
		 ON CONFLICT(endpoint) DO UPDATE SET user_id = excluded.user_id, login_hash = excluded.login_hash,
		   p256dh = excluded.p256dh, auth = excluded.auth, user_agent = excluded.user_agent, failures = 0`,
		p.Endpoint, p.P256dh, p.Auth, p.UserAgent, s.unix(), p.LoginHash, p.UserID)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE user_id = ? AND id NOT IN
		   (SELECT id FROM push_subscriptions WHERE user_id = ? ORDER BY id DESC LIMIT ?)`, p.UserID, p.UserID, keep); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PushSubscriptions(ctx context.Context, userID int64) ([]*PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, COALESCE(login_hash, ''), endpoint, p256dh, auth, user_agent, created_at, last_ok_at
		 FROM push_subscriptions WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PushSubscription{}
	for rows.Next() {
		p := &PushSubscription{}
		if err := rows.Scan(&p.ID, &p.UserID, &p.LoginHash, &p.Endpoint, &p.P256dh, &p.Auth, &p.UserAgent, &p.CreatedAt, &p.LastOKAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeletePushSubscription(ctx context.Context, userID int64, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE user_id = ? AND endpoint = ?`, userID, endpoint)
	return err
}

func (s *Store) DeletePushSubscriptionByID(ctx context.Context, userID, id int64) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE user_id = ? AND id = ?`, userID, id))
}

func (s *Store) MarkPushResult(ctx context.Context, id int64, ok bool) error {
	if ok {
		_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_ok_at = ?, failures = 0 WHERE id = ?`, s.unix(), id)
		return err
	}
	// Endpoints that keep failing are dropped; the browser re-subscribes on next visit.
	if _, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET failures = failures + 1 WHERE id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ? AND failures >= 5`, id)
	return err
}

func (s *Store) DeletePushSubscriptionHard(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id)
	return err
}

// Forwards ---------------------------------------------------------------

const (
	SharePrivate = "private" // only the owner
	ShareUsers   = "users"   // anyone signed in to this Corral
)

type Forward struct {
	ID        string `json:"id"`
	OwnerID   int64  `json:"-"`
	HostID    int64  `json:"host_id"`
	Port      int    `json:"port"`
	Sharing   string `json:"sharing"`
	CreatedAt int64  `json:"created_at"`
}

const forwardAlphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no look-alikes (l, o, 0, 1)

func newForwardID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(forwardAlphabet[int(c)%len(forwardAlphabet)])
	}
	return sb.String(), nil
}

// EnsureForward returns the forward for (owner, host, port), creating it with
// a fresh random id if needed.
func (s *Store) EnsureForward(ctx context.Context, ownerID, hostID int64, port int) (*Forward, error) {
	if f, err := s.forwardFor(ctx, ownerID, hostID, port); err == nil {
		return f, nil
	}
	for i := 0; i < 8; i++ {
		id, err := newForwardID()
		if err != nil {
			return nil, err
		}
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO forwards (id, owner_id, host_id, port, created_at)
			 SELECT ?, ?, id, ?, ? FROM hosts WHERE owner_id = ? AND id = ?`, id, ownerID, port, s.unix(), ownerID, hostID)
		if err == nil {
			return s.forwardFor(ctx, ownerID, hostID, port)
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			return nil, err
		}
		if f, err := s.forwardFor(ctx, ownerID, hostID, port); err == nil {
			return f, nil // created concurrently
		}
	}
	return nil, errors.New("could not allocate a forward id")
}

func scanForward(row interface{ Scan(...any) error }) (*Forward, error) {
	f := &Forward{}
	if err := row.Scan(&f.ID, &f.OwnerID, &f.HostID, &f.Port, &f.Sharing, &f.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return f, nil
}

func (s *Store) forwardFor(ctx context.Context, ownerID, hostID int64, port int) (*Forward, error) {
	return scanForward(s.db.QueryRowContext(ctx,
		`SELECT id, owner_id, host_id, port, sharing, created_at FROM forwards WHERE owner_id = ? AND host_id = ? AND port = ?`, ownerID, hostID, port))
}

// ForwardByID looks a forward up by its public id; callers must check access.
func (s *Store) ForwardByID(ctx context.Context, id string) (*Forward, error) {
	return scanForward(s.db.QueryRowContext(ctx,
		`SELECT id, owner_id, host_id, port, sharing, created_at FROM forwards WHERE id = ?`, id))
}

func (s *Store) ListForwards(ctx context.Context, ownerID int64) ([]*Forward, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, owner_id, host_id, port, sharing, created_at FROM forwards WHERE owner_id = ? ORDER BY created_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Forward{}
	for rows.Next() {
		f, err := scanForward(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) SetForwardSharing(ctx context.Context, ownerID int64, id, sharing string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `UPDATE forwards SET sharing = ? WHERE owner_id = ? AND id = ?`, sharing, ownerID, id))
}

func (s *Store) DeleteForward(ctx context.Context, ownerID int64, id string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `DELETE FROM forwards WHERE owner_id = ? AND id = ?`, ownerID, id))
}
