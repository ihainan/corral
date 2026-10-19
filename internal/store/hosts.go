package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const (
	AuthPassword = "password" // stored password credential
	AuthKey      = "key"      // stored private key credential
	AuthAsk      = "ask"      // password typed at connect time, never stored

	PersistNone  = "none"
	PersistTmux  = "tmux"
	PersistHerdr = "herdr"
)

type Host struct {
	ID              int64  `json:"id"`
	OwnerID         int64  `json:"-"`
	Name            string `json:"name"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	Auth            string `json:"auth"`
	CredentialID    *int64 `json:"credential_id"`
	JumpHostID      *int64 `json:"jump_host_id"` // reached through this host of the same owner
	Persistence     string `json:"persistence"`
	TmuxSession     string `json:"tmux_session"`
	HerdrSession    string `json:"herdr_session"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	LastConnectedAt int64  `json:"last_connected_at"`
	// Version changes with every saved edit; an edit is only saved while
	// the host is still at the version it was loaded at.
	Version int64 `json:"version"`
	// ConnVersion changes with every edit to what a connection to the host
	// is (target, user, authentication, saved password) or the key it trusts.
	ConnVersion int64 `json:"-"`
}

const hostCols = `id, owner_id, name, address, port, username, auth, credential_id, persistence, tmux_session, herdr_session, created_at, updated_at, last_connected_at, conn_version, version, jump_host_id`

func scanHost(row interface{ Scan(...any) error }) (*Host, error) {
	h := &Host{}
	var cred, jump sql.NullInt64
	err := row.Scan(&h.ID, &h.OwnerID, &h.Name, &h.Address, &h.Port, &h.Username, &h.Auth, &cred,
		&h.Persistence, &h.TmuxSession, &h.HerdrSession, &h.CreatedAt, &h.UpdatedAt, &h.LastConnectedAt, &h.ConnVersion, &h.Version, &jump)
	if err != nil {
		return nil, notFound(err)
	}
	if cred.Valid {
		h.CredentialID = &cred.Int64
	}
	if jump.Valid {
		h.JumpHostID = &jump.Int64
	}
	return h, nil
}

func (s *Store) ListHosts(ctx context.Context, ownerID int64) ([]*Host, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+hostCols+` FROM hosts WHERE owner_id = ? ORDER BY name COLLATE NOCASE, id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Host{}
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) Host(ctx context.Context, ownerID, id int64) (*Host, error) {
	return scanHost(s.db.QueryRowContext(ctx, `SELECT `+hostCols+` FROM hosts WHERE owner_id = ? AND id = ?`, ownerID, id))
}

// ErrJumpConflict: the jump host chosen cannot be one (any more): it is
// itself reached through another host, has no saved credential, or the host
// saved has others reached through it. Checked in the same statement as the
// save, so concurrent edits cannot build a chain.
var ErrJumpConflict = errors.New("the jump host cannot be used")

// jumpOK is the SQL condition (with its arguments) that a host (id; 0 for a
// new one) with jump host jump and authentication auth keeps jump hosts one
// level deep, each with a saved credential.
func jumpOK(ownerID, id int64, jump *int64, auth string) (string, []any) {
	return `(? IS NULL OR (? != ? AND EXISTS (SELECT 1 FROM hosts j WHERE j.owner_id = ? AND j.id = ? AND j.jump_host_id IS NULL
		       AND j.auth != 'ask' AND j.credential_id IS NOT NULL)))
		 AND ((? IS NULL AND ? != 'ask') OR NOT EXISTS (SELECT 1 FROM hosts d WHERE d.owner_id = ? AND d.jump_host_id = ?))`,
		[]any{jump, jump, id, ownerID, jump, jump, auth, ownerID, id}
}

// CreateHost saves a new host. On an error, the host returned is non-nil
// only if it was saved (and reading it back failed).
func (s *Store) CreateHost(ctx context.Context, h *Host) (*Host, error) {
	now := s.unix()
	cond, args := jumpOK(h.OwnerID, 0, h.JumpHostID, h.Auth)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO hosts (owner_id, name, address, port, username, auth, credential_id, persistence, tmux_session, herdr_session, created_at, updated_at, jump_host_id)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE `+cond,
		append([]any{h.OwnerID, h.Name, h.Address, h.Port, h.Username, h.Auth, h.CredentialID, h.Persistence, h.TmuxSession, h.HerdrSession, now, now, h.JumpHostID}, args...)...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrJumpConflict
	}
	// Saved: from here on a host is returned, also if reading it back fails.
	id, _ := res.LastInsertId()
	created, err := s.Host(ctx, h.OwnerID, id)
	if err != nil {
		saved := *h
		saved.ID = id
		return &saved, err
	}
	return created, nil
}

// UpdateHost saves a host if it is still at h.Version (ErrNotFound if not)
// and bumps that; with newConn it also bumps the connection version, its
// own and that of the hosts reached through it.
func (s *Store) UpdateHost(ctx context.Context, h *Host, newConn bool) error {
	bump := 0
	if newConn {
		bump = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cond, args := jumpOK(h.OwnerID, h.ID, h.JumpHostID, h.Auth)
	res, err := tx.ExecContext(ctx,
		`UPDATE hosts SET name = ?, address = ?, port = ?, username = ?, auth = ?, credential_id = ?, persistence = ?, tmux_session = ?,
		   herdr_session = ?, jump_host_id = ?, updated_at = ?, conn_version = conn_version + ?, version = version + 1
		 WHERE owner_id = ? AND id = ? AND version = ? AND `+cond,
		append([]any{h.Name, h.Address, h.Port, h.Username, h.Auth, h.CredentialID, h.Persistence, h.TmuxSession, h.HerdrSession, h.JumpHostID, s.unix(), bump,
			h.OwnerID, h.ID, h.Version}, args...)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Changed or gone since (ErrNotFound), or the jump hosts no longer allow it.
		var v int64
		if err := tx.QueryRowContext(ctx, `SELECT version FROM hosts WHERE owner_id = ? AND id = ?`, h.OwnerID, h.ID).Scan(&v); err != nil || v != h.Version {
			return ErrNotFound
		}
		return ErrJumpConflict
	}
	if newConn {
		if _, err := tx.ExecContext(ctx, `UPDATE hosts SET conn_version = conn_version + 1 WHERE owner_id = ? AND jump_host_id = ?`, h.OwnerID, h.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BumpConnVersion marks every existing connection to the host as outdated,
// and those to the hosts reached through it.
func (s *Store) BumpConnVersion(ctx context.Context, ownerID, id int64) error {
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`UPDATE hosts SET conn_version = conn_version + 1 WHERE owner_id = ? AND (id = ? OR jump_host_id = ?)`, ownerID, id, id))
}

// ErrInUse: other hosts are reached through this one.
var ErrInUse = errors.New("other hosts are reached through this host")

func (s *Store) DeleteHost(ctx context.Context, ownerID, id int64) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts WHERE owner_id = ? AND jump_host_id = ?`, ownerID, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrInUse
	}
	err := affectedOrNotFound(s.db.ExecContext(ctx, `DELETE FROM hosts WHERE owner_id = ? AND id = ?`, ownerID, id))
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		return ErrInUse // (one was set to go through it meanwhile)
	}
	return err
}

// HostsThrough lists the names of the owner's hosts reached through a host.
func (s *Store) HostsThrough(ctx context.Context, ownerID, id int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM hosts WHERE owner_id = ? AND jump_host_id = ? ORDER BY name COLLATE NOCASE`, ownerID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) TouchHostConnected(ctx context.Context, ownerID, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET last_connected_at = ? WHERE owner_id = ? AND id = ?`, s.unix(), ownerID, id)
	return err
}

// Host keys (trust on first use) -------------------------------------------

type HostKey struct {
	KeyType     string `json:"key_type"`
	KeyData     []byte `json:"-"`
	Fingerprint string `json:"fingerprint"`
	FirstSeen   int64  `json:"first_seen"`
}

// HostKey returns the pinned key for a host the caller owns.
func (s *Store) HostKey(ctx context.Context, ownerID, hostID int64) (*HostKey, error) {
	k := &HostKey{}
	err := s.db.QueryRowContext(ctx,
		`SELECT k.key_type, k.key_data, k.fingerprint, k.first_seen FROM host_keys k JOIN hosts h ON h.id = k.host_id
		 WHERE h.owner_id = ? AND k.host_id = ?`, ownerID, hostID).
		Scan(&k.KeyType, &k.KeyData, &k.Fingerprint, &k.FirstSeen)
	if err != nil {
		return nil, notFound(err)
	}
	return k, nil
}

// PinHostKey stores a key only if the host has none yet (first use).
func (s *Store) PinHostKey(ctx context.Context, ownerID, hostID int64, k *HostKey) error {
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`INSERT INTO host_keys (host_id, key_type, key_data, fingerprint, first_seen)
		 SELECT id, ?, ?, ?, ? FROM hosts WHERE owner_id = ? AND id = ?
		 ON CONFLICT(host_id) DO NOTHING`,
		k.KeyType, k.KeyData, k.Fingerprint, s.unix(), ownerID, hostID))
}

// ReplaceHostKey swaps the pinned key, but only if it is still the one the
// user saw as "old" when accepting the change.
func (s *Store) ReplaceHostKey(ctx context.Context, ownerID, hostID int64, oldFingerprint string, k *HostKey) error {
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`UPDATE host_keys SET key_type = ?, key_data = ?, fingerprint = ?, first_seen = ?
		 WHERE host_id = (SELECT id FROM hosts WHERE owner_id = ? AND id = ?) AND fingerprint = ?`,
		k.KeyType, k.KeyData, k.Fingerprint, s.unix(), ownerID, hostID, oldFingerprint))
}

// ForgetHostKey drops the pinned key so the next connection pins a new one.
func (s *Store) ForgetHostKey(ctx context.Context, ownerID, hostID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM host_keys WHERE host_id = (SELECT id FROM hosts WHERE owner_id = ? AND id = ?)`, ownerID, hostID)
	return err
}

// Credentials --------------------------------------------------------------

const (
	CredPassword   = "password"
	CredPrivateKey = "private_key"
)

type Credential struct {
	ID         int64  `json:"id"`
	OwnerID    int64  `json:"-"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	PublicKey  string `json:"public_key"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	Ciphertext []byte `json:"-"`
	WrappedKey []byte `json:"-"`
	KeyID      string `json:"-"`
}

const credCols = `id, owner_id, name, kind, public_key, ciphertext, wrapped_key, key_id, created_at, updated_at`

func scanCred(row interface{ Scan(...any) error }) (*Credential, error) {
	c := &Credential{}
	err := row.Scan(&c.ID, &c.OwnerID, &c.Name, &c.Kind, &c.PublicKey, &c.Ciphertext, &c.WrappedKey, &c.KeyID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

func (s *Store) ListCredentials(ctx context.Context, ownerID int64) ([]*Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+credCols+` FROM credentials WHERE owner_id = ? ORDER BY name COLLATE NOCASE, id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Credential{}
	for rows.Next() {
		c, err := scanCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Credential(ctx context.Context, ownerID, id int64) (*Credential, error) {
	return scanCred(s.db.QueryRowContext(ctx, `SELECT `+credCols+` FROM credentials WHERE owner_id = ? AND id = ?`, ownerID, id))
}

// ReserveCredential inserts a placeholder row so the caller learns the id it must
// bind into the ciphertext's associated data, then FillCredential stores the secret.
func (s *Store) ReserveCredential(ctx context.Context, ownerID int64, name, kind, publicKey string) (int64, error) {
	now := s.unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (owner_id, name, kind, public_key, ciphertext, wrapped_key, key_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, x'', x'', '', ?, ?)`, ownerID, name, kind, publicKey, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FillCredential(ctx context.Context, ownerID, id int64, ciphertext, wrappedKey []byte, keyID string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`UPDATE credentials SET ciphertext = ?, wrapped_key = ?, key_id = ?, updated_at = ? WHERE owner_id = ? AND id = ?`,
		ciphertext, wrappedKey, keyID, s.unix(), ownerID, id))
}

func (s *Store) RenameCredential(ctx context.Context, ownerID, id int64, name string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`UPDATE credentials SET name = ?, updated_at = ? WHERE owner_id = ? AND id = ?`, name, s.unix(), ownerID, id))
}

func (s *Store) DeleteCredential(ctx context.Context, ownerID, id int64) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `DELETE FROM credentials WHERE owner_id = ? AND id = ?`, ownerID, id))
}
