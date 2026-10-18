package store

import (
	"context"
	"database/sql"
)

const (
	AuthPassword = "password" // stored password credential
	AuthKey      = "key"      // stored private key credential
	AuthAsk      = "ask"      // password typed at connect time, never stored

	PersistNone = "none"
	PersistTmux = "tmux"
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
	Persistence     string `json:"persistence"`
	TmuxSession     string `json:"tmux_session"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	LastConnectedAt int64  `json:"last_connected_at"`
}

const hostCols = `id, owner_id, name, address, port, username, auth, credential_id, persistence, tmux_session, created_at, updated_at, last_connected_at`

func scanHost(row interface{ Scan(...any) error }) (*Host, error) {
	h := &Host{}
	var cred sql.NullInt64
	err := row.Scan(&h.ID, &h.OwnerID, &h.Name, &h.Address, &h.Port, &h.Username, &h.Auth, &cred,
		&h.Persistence, &h.TmuxSession, &h.CreatedAt, &h.UpdatedAt, &h.LastConnectedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if cred.Valid {
		h.CredentialID = &cred.Int64
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

func (s *Store) CreateHost(ctx context.Context, h *Host) (*Host, error) {
	now := s.unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO hosts (owner_id, name, address, port, username, auth, credential_id, persistence, tmux_session, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.OwnerID, h.Name, h.Address, h.Port, h.Username, h.Auth, h.CredentialID, h.Persistence, h.TmuxSession, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Host(ctx, h.OwnerID, id)
}

func (s *Store) UpdateHost(ctx context.Context, h *Host) error {
	return affectedOrNotFound(s.db.ExecContext(ctx,
		`UPDATE hosts SET name = ?, address = ?, port = ?, username = ?, auth = ?, credential_id = ?, persistence = ?, tmux_session = ?, updated_at = ?
		 WHERE owner_id = ? AND id = ?`,
		h.Name, h.Address, h.Port, h.Username, h.Auth, h.CredentialID, h.Persistence, h.TmuxSession, s.unix(), h.OwnerID, h.ID))
}

func (s *Store) DeleteHost(ctx context.Context, ownerID, id int64) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `DELETE FROM hosts WHERE owner_id = ? AND id = ?`, ownerID, id))
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
