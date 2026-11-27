package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// ErrOpInProgress is returned when a host already has an unfinished corrald operation.
var ErrOpInProgress = errors.New("another corrald operation is already in progress")

// CorraldHost persists the gateway's view of corrald consent, binding, and state
// for a single Corral host record.
type CorraldHost struct {
	HostID           int64
	State            string
	ConsentBy        *int64
	ConsentAt        int64
	BoundFingerprint string
	BoundUsername    string
	BoundGateway     string
	ManifestJSON     string
	Version          string
	Release          int64
	MachineID        string
	SocketPath       string
	Files            []string
	PausedUsers      []int64
	LastOKAt         int64
	LastError        string
	RestartCount     int
	UpdatedAt        int64
}

// CorraldOp persists a single install/update/pause/resume/remove operation.
type CorraldOp struct {
	ID        string
	HostID    int64
	Kind      string
	State     string
	Step      string
	ErrorCode string
	Detail    string
	CreatedBy int64
	CreatedAt int64
	UpdatedAt int64
}

// CorraldPeerHost describes another Corral host record bound to the same
// (fingerprint, username) on this gateway.
type CorraldPeerHost struct {
	HostID      int64
	OwnerID     int64
	Username    string // Corral username (from users.username)
	DisplayName string // Corral display_name
	State       string
	PausedUsers []int64
}

const corraldHostCols = `host_id, state, consent_by, consent_at, bound_fingerprint, bound_username, bound_gateway,
	manifest_json, version, release, machine_id, socket_path, files_json, paused_users_json, last_ok_at, last_error, restart_count, updated_at`

func scanCorraldHost(row interface{ Scan(...any) error }) (*CorraldHost, error) {
	ch := &CorraldHost{}
	var consentBy sql.NullInt64
	var filesJSON, pausedJSON string
	err := row.Scan(
		&ch.HostID, &ch.State, &consentBy, &ch.ConsentAt,
		&ch.BoundFingerprint, &ch.BoundUsername, &ch.BoundGateway,
		&ch.ManifestJSON, &ch.Version, &ch.Release,
		&ch.MachineID, &ch.SocketPath, &filesJSON, &pausedJSON,
		&ch.LastOKAt, &ch.LastError, &ch.RestartCount, &ch.UpdatedAt,
	)
	if err != nil {
		return nil, notFound(err)
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
	return ch, nil
}

// CorraldHost returns the corrald state row for a host owned by ownerID.
// Returns ErrNotFound if the host does not exist or has no corrald_hosts row yet.
func (s *Store) CorraldHost(ctx context.Context, ownerID, hostID int64) (*CorraldHost, error) {
	return scanCorraldHost(s.db.QueryRowContext(ctx,
		`SELECT ch.host_id, ch.state, ch.consent_by, ch.consent_at, ch.bound_fingerprint, ch.bound_username, ch.bound_gateway,
			ch.manifest_json, ch.version, ch.release, ch.machine_id, ch.socket_path, ch.files_json, ch.paused_users_json,
			ch.last_ok_at, ch.last_error, ch.restart_count, ch.updated_at
		 FROM corrald_hosts ch JOIN hosts h ON h.id = ch.host_id
		 WHERE h.owner_id = ? AND ch.host_id = ?`, ownerID, hostID))
}

// CorraldHostByID returns the corrald state row for hostID (internal manager use).
func (s *Store) CorraldHostByID(ctx context.Context, hostID int64) (*CorraldHost, error) {
	return scanCorraldHost(s.db.QueryRowContext(ctx,
		`SELECT `+corraldHostCols+` FROM corrald_hosts WHERE host_id = ?`, hostID))
}

// UpsertCorraldHost creates or updates the corrald_hosts row for ch.HostID.
func (s *Store) UpsertCorraldHost(ctx context.Context, ch *CorraldHost) error {
	now := s.unix()
	ch.UpdatedAt = now
	files := ch.Files
	if files == nil {
		files = []string{}
	}
	paused := ch.PausedUsers
	if paused == nil {
		paused = []int64{}
	}
	filesB, err := json.Marshal(files)
	if err != nil {
		return err
	}
	pausedB, err := json.Marshal(paused)
	if err != nil {
		return err
	}
	var consentBy any
	if ch.ConsentBy != nil {
		consentBy = *ch.ConsentBy
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO corrald_hosts (
			host_id, state, consent_by, consent_at, bound_fingerprint, bound_username, bound_gateway,
			manifest_json, version, release, machine_id, socket_path, files_json, paused_users_json,
			last_ok_at, last_error, restart_count, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(host_id) DO UPDATE SET
			state = excluded.state,
			consent_by = excluded.consent_by,
			consent_at = excluded.consent_at,
			bound_fingerprint = excluded.bound_fingerprint,
			bound_username = excluded.bound_username,
			bound_gateway = excluded.bound_gateway,
			manifest_json = excluded.manifest_json,
			version = excluded.version,
			release = excluded.release,
			machine_id = excluded.machine_id,
			socket_path = excluded.socket_path,
			files_json = excluded.files_json,
			paused_users_json = excluded.paused_users_json,
			last_ok_at = excluded.last_ok_at,
			last_error = excluded.last_error,
			restart_count = excluded.restart_count,
			updated_at = excluded.updated_at`,
		ch.HostID, ch.State, consentBy, ch.ConsentAt,
		ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway,
		ch.ManifestJSON, ch.Version, ch.Release,
		ch.MachineID, ch.SocketPath, string(filesB), string(pausedB),
		ch.LastOKAt, ch.LastError, ch.RestartCount, now,
	)
	return err
}

// UpdateCorraldFingerprint updates bound_fingerprint on host key rotation, or marks
// the host as rebind when keepAuth is false. Only acts if a corrald_hosts row exists
// and is not off/removing/remove_pending.
func (s *Store) UpdateCorraldFingerprint(ctx context.Context, hostID int64, newFingerprint string, keepAuth bool) error {
	now := s.unix()
	if keepAuth {
		_, err := s.db.ExecContext(ctx,
			`UPDATE corrald_hosts SET bound_fingerprint = ?, updated_at = ?
			 WHERE host_id = ? AND state NOT IN ('off', 'removing', 'remove_pending')`,
			newFingerprint, now, hostID)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE corrald_hosts SET state = 'rebind', updated_at = ?
		 WHERE host_id = ? AND state NOT IN ('off', 'removing', 'remove_pending')`,
		now, hostID)
	return err
}

// MarkCorraldRebind transitions an enabled host to rebind (e.g. when endpoint or username changed).
func (s *Store) MarkCorraldRebind(ctx context.Context, hostID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE corrald_hosts SET state = 'rebind', updated_at = ?
		 WHERE host_id = ? AND state NOT IN ('off', 'removing', 'remove_pending', 'rebind')`,
		s.unix(), hostID)
	return err
}

// CorraldPeersByBinding returns all Corral host records on this gateway that share
// the same (bound_fingerprint, bound_username) and are not in 'off' state.
func (s *Store) CorraldPeersByBinding(ctx context.Context, fingerprint, username string) ([]CorraldPeerHost, error) {
	if fingerprint == "" || username == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ch.host_id, h.owner_id, u.username, u.display_name, ch.state, ch.paused_users_json
		FROM corrald_hosts ch
		JOIN hosts h ON h.id = ch.host_id
		JOIN users u ON u.id = h.owner_id
		WHERE ch.bound_fingerprint = ? AND ch.bound_username = ? AND ch.state != 'off'
		ORDER BY ch.host_id`, fingerprint, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CorraldPeerHost
	for rows.Next() {
		var p CorraldPeerHost
		var pausedJSON string
		if err := rows.Scan(&p.HostID, &p.OwnerID, &p.Username, &p.DisplayName, &p.State, &pausedJSON); err != nil {
			return nil, err
		}
		if pausedJSON != "" {
			_ = json.Unmarshal([]byte(pausedJSON), &p.PausedUsers)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CorraldResetByBinding resets all corrald_hosts rows sharing (fingerprint, username)
// to 'off' after an uninstall removes the shared daemon on the host.
func (s *Store) CorraldResetByBinding(ctx context.Context, fingerprint, username string) error {
	if fingerprint == "" || username == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = 'off', version = '', release = 0, socket_path = '', files_json = '[]',
		    paused_users_json = '[]', last_error = '', restart_count = 0, updated_at = ?
		WHERE bound_fingerprint = ? AND bound_username = ?`,
		s.unix(), fingerprint, username)
	return err
}

// ListUserCorraldHosts returns all hosts owned by userID that have a non-off corrald_hosts row.
func (s *Store) ListUserCorraldHosts(ctx context.Context, userID int64) ([]*Host, []*CorraldHost, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.owner_id, h.name, h.address, h.port, h.username, h.auth, h.credential_id,
		       h.persistence, h.tmux_session, h.herdr_session, h.created_at, h.updated_at, h.last_connected_at,
		       h.conn_version, h.version, h.jump_host_id,
		       ch.host_id, ch.state, ch.consent_by, ch.consent_at, ch.bound_fingerprint, ch.bound_username,
		       ch.bound_gateway, ch.manifest_json, ch.version, ch.release, ch.machine_id, ch.socket_path,
		       ch.files_json, ch.paused_users_json, ch.last_ok_at, ch.last_error, ch.restart_count, ch.updated_at
		FROM hosts h
		JOIN corrald_hosts ch ON ch.host_id = h.id
		WHERE h.owner_id = ? AND ch.state != 'off'
		ORDER BY h.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var hosts []*Host
	var states []*CorraldHost
	for rows.Next() {
		h := &Host{}
		ch := &CorraldHost{}
		var cred, jump, consentBy sql.NullInt64
		var filesJSON, pausedJSON string
		if err := rows.Scan(
			&h.ID, &h.OwnerID, &h.Name, &h.Address, &h.Port, &h.Username, &h.Auth, &cred,
			&h.Persistence, &h.TmuxSession, &h.HerdrSession, &h.CreatedAt, &h.UpdatedAt, &h.LastConnectedAt,
			&h.ConnVersion, &h.Version, &jump,
			&ch.HostID, &ch.State, &consentBy, &ch.ConsentAt,
			&ch.BoundFingerprint, &ch.BoundUsername, &ch.BoundGateway,
			&ch.ManifestJSON, &ch.Version, &ch.Release,
			&ch.MachineID, &ch.SocketPath, &filesJSON, &pausedJSON,
			&ch.LastOKAt, &ch.LastError, &ch.RestartCount, &ch.UpdatedAt,
		); err != nil {
			return nil, nil, err
		}
		if cred.Valid {
			v := cred.Int64
			h.CredentialID = &v
		}
		if jump.Valid {
			v := jump.Int64
			h.JumpHostID = &v
		}
		if consentBy.Valid {
			v := consentBy.Int64
			ch.ConsentBy = &v
		}
		if filesJSON != "" {
			_ = json.Unmarshal([]byte(filesJSON), &ch.Files)
		}
		if pausedJSON != "" {
			_ = json.Unmarshal([]byte(pausedJSON), &ch.PausedUsers)
		}
		hosts = append(hosts, h)
		states = append(states, ch)
	}
	return hosts, states, rows.Err()
}

// ListIncompleteCorraldOps returns all operations still marked pending or running
// (used after a gateway restart to reconcile host state).
func (s *Store) ListIncompleteCorraldOps(ctx context.Context) ([]*CorraldOp, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, host_id, kind, state, step, error_code, detail, created_by, created_at, updated_at
		FROM corrald_ops WHERE state IN ('pending', 'running') ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CorraldOp
	for rows.Next() {
		op := &CorraldOp{}
		if err := rows.Scan(&op.ID, &op.HostID, &op.Kind, &op.State, &op.Step, &op.ErrorCode, &op.Detail, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// CreateCorraldOp inserts a new operation. If another pending/running operation
// already exists for op.HostID, ErrOpInProgress is returned.
// When supersedeInstallUpdate is true (for withdrawal-first remove), any active
// install or update op for the host is cancelled atomically in the same transaction.
func (s *Store) CreateCorraldOp(ctx context.Context, op *CorraldOp, supersedeInstallUpdate bool) error {
	now := s.unix()
	op.CreatedAt = now
	op.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if supersedeInstallUpdate {
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_ops
			SET state = 'cancelled', error_code = 'removing', updated_at = ?
			WHERE host_id = ? AND state IN ('pending', 'running') AND kind IN ('install', 'update')`,
			now, op.HostID); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO corrald_ops (id, host_id, kind, state, step, error_code, detail, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.HostID, op.Kind, op.State, op.Step, op.ErrorCode, op.Detail, op.CreatedBy, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrOpInProgress
		}
		return err
	}
	return tx.Commit()
}

// UpdateCorraldOp updates state, step, error_code, and detail for an operation.
func (s *Store) UpdateCorraldOp(ctx context.Context, op *CorraldOp) error {
	op.UpdatedAt = s.unix()
	return affectedOrNotFound(s.db.ExecContext(ctx, `
		UPDATE corrald_ops SET state = ?, step = ?, error_code = ?, detail = ?, updated_at = ?
		WHERE id = ?`,
		op.State, op.Step, op.ErrorCode, op.Detail, op.UpdatedAt, op.ID))
}

// LatestCorraldOp returns the most recent operation for hostID, or ErrNotFound.
func (s *Store) LatestCorraldOp(ctx context.Context, hostID int64) (*CorraldOp, error) {
	op := &CorraldOp{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, host_id, kind, state, step, error_code, detail, created_by, created_at, updated_at
		FROM corrald_ops WHERE host_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, hostID).
		Scan(&op.ID, &op.HostID, &op.Kind, &op.State, &op.Step, &op.ErrorCode, &op.Detail, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return op, nil
}

// ActiveCorraldOp returns the currently pending/running operation for hostID, or ErrNotFound.
func (s *Store) ActiveCorraldOp(ctx context.Context, hostID int64) (*CorraldOp, error) {
	op := &CorraldOp{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, host_id, kind, state, step, error_code, detail, created_by, created_at, updated_at
		FROM corrald_ops WHERE host_id = ? AND state IN ('pending', 'running') LIMIT 1`, hostID).
		Scan(&op.ID, &op.HostID, &op.Kind, &op.State, &op.Step, &op.ErrorCode, &op.Detail, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return op, nil
}

// SetCorraldUserPaused adds or removes userID in paused_users_json for hostID.
func (s *Store) SetCorraldUserPaused(ctx context.Context, hostID, userID int64, paused bool) (*CorraldHost, error) {
	ch, err := s.CorraldHostByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	has := slices.Contains(ch.PausedUsers, userID)
	if paused && !has {
		ch.PausedUsers = append(ch.PausedUsers, userID)
	} else if !paused && has {
		next := make([]int64, 0, len(ch.PausedUsers)-1)
		for _, id := range ch.PausedUsers {
			if id != userID {
				next = append(next, id)
			}
		}
		ch.PausedUsers = next
	}
	if err := s.UpsertCorraldHost(ctx, ch); err != nil {
		return nil, err
	}
	return ch, nil
}
