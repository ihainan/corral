package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// ErrOpInProgress is returned when a host or target account already has an
// unfinished corrald operation.
var ErrOpInProgress = errors.New("another corrald operation is already in progress")

// CorraldAccountKey formats the canonical (fingerprint, username) exclusivity
// key for corrald_ops. Returns "" when either component is missing.
func CorraldAccountKey(fingerprint, username string) string {
	if fingerprint == "" || username == "" {
		return ""
	}
	return fingerprint + "\x00" + username
}

// CorraldHost persists the gateway's view of corrald consent, binding, and state
// for a single Corral host record.
type CorraldHost struct {
	HostID            int64
	State             string
	ConsentBy         *int64
	ConsentAt         int64
	BoundFingerprint  string
	BoundUsername     string
	BoundGateway      string
	ManifestJSON      string
	Version           string
	Release           int64
	MachineID         string
	SocketPath        string
	Files             []string
	PausedUsers       []int64
	FailedReleases    []int64
	ActiveOpID        string
	UpdatedFromVer    string
	UpdatedFromRel    int64
	UpdatedFromTarget string
	UpdateTargetVer   string
	UpdateTargetRel   int64
	UpdateReadyAt     int64
	UpdateCrashCount  int
	LastOKAt          int64
	LastError         string
	RestartCount      int
	UpdatedAt         int64
}

// CorraldOp persists a single install/update/pause/resume/remove operation.
type CorraldOp struct {
	ID         string
	HostID     int64
	AccountKey string
	Kind       string
	State      string
	Step       string
	ErrorCode  string
	Detail     string
	CreatedBy  int64
	CreatedAt  int64
	UpdatedAt  int64
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
	manifest_json, version, release, machine_id, socket_path, files_json, paused_users_json, failed_releases_json,
	active_op_id, updated_from_ver, updated_from_rel, updated_from_target, update_target_ver, update_target_rel,
	update_ready_at, update_crash_count, last_ok_at, last_error, restart_count, updated_at`

const corraldHostQualifiedCols = `ch.host_id, ch.state, ch.consent_by, ch.consent_at, ch.bound_fingerprint, ch.bound_username, ch.bound_gateway,
	ch.manifest_json, ch.version, ch.release, ch.machine_id, ch.socket_path, ch.files_json, ch.paused_users_json, ch.failed_releases_json,
	ch.active_op_id, ch.updated_from_ver, ch.updated_from_rel, ch.updated_from_target, ch.update_target_ver, ch.update_target_rel,
	ch.update_ready_at, ch.update_crash_count, ch.last_ok_at, ch.last_error, ch.restart_count, ch.updated_at`

func scanCorraldHost(row interface{ Scan(...any) error }) (*CorraldHost, error) {
	ch := &CorraldHost{}
	var consentBy sql.NullInt64
	var filesJSON, pausedJSON, failedJSON string
	err := row.Scan(
		&ch.HostID, &ch.State, &consentBy, &ch.ConsentAt,
		&ch.BoundFingerprint, &ch.BoundUsername, &ch.BoundGateway,
		&ch.ManifestJSON, &ch.Version, &ch.Release,
		&ch.MachineID, &ch.SocketPath, &filesJSON, &pausedJSON, &failedJSON,
		&ch.ActiveOpID, &ch.UpdatedFromVer, &ch.UpdatedFromRel, &ch.UpdatedFromTarget,
		&ch.UpdateTargetVer, &ch.UpdateTargetRel, &ch.UpdateReadyAt, &ch.UpdateCrashCount,
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
	if failedJSON != "" {
		_ = json.Unmarshal([]byte(failedJSON), &ch.FailedReleases)
	}
	if ch.FailedReleases == nil {
		ch.FailedReleases = []int64{}
	}
	return ch, nil
}

// RecordSSHVerified records that hostID completed a real SSH authentication to
// (fingerprint, username). Shared corrald metadata is only exposed to users
// whose host record has been verified for its current (fingerprint, username).
func (s *Store) RecordSSHVerified(ctx context.Context, hostID int64, fingerprint, username string) error {
	if hostID <= 0 || fingerprint == "" || username == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO corrald_ssh_verified (host_id, fingerprint, username, verified_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(host_id) DO UPDATE SET
			fingerprint = excluded.fingerprint,
			username = excluded.username,
			verified_at = excluded.verified_at`,
		hostID, fingerprint, username, s.unix())
	return err
}

// IsSSHVerified reports whether hostID has previously completed an SSH login
// for the exact (fingerprint, username) pair.
func (s *Store) IsSSHVerified(ctx context.Context, hostID int64, fingerprint, username string) bool {
	if hostID <= 0 || fingerprint == "" || username == "" {
		return false
	}
	var gotFP, gotUser string
	err := s.db.QueryRowContext(ctx,
		`SELECT fingerprint, username FROM corrald_ssh_verified WHERE host_id = ?`, hostID).
		Scan(&gotFP, &gotUser)
	if err != nil {
		return false
	}
	return gotFP == fingerprint && gotUser == username
}

// CorraldHost returns the corrald state row for a host owned by ownerID.
// Returns ErrNotFound if the host does not exist or has no corrald_hosts row yet.
func (s *Store) CorraldHost(ctx context.Context, ownerID, hostID int64) (*CorraldHost, error) {
	return scanCorraldHost(s.db.QueryRowContext(ctx,
		`SELECT `+corraldHostQualifiedCols+`
		 FROM corrald_hosts ch JOIN hosts h ON h.id = ch.host_id
		 WHERE h.owner_id = ? AND ch.host_id = ?`, ownerID, hostID))
}

// CorraldHostByID returns the corrald state row for hostID (internal manager use).
func (s *Store) CorraldHostByID(ctx context.Context, hostID int64) (*CorraldHost, error) {
	return scanCorraldHost(s.db.QueryRowContext(ctx,
		`SELECT `+corraldHostCols+` FROM corrald_hosts WHERE host_id = ?`, hostID))
}

func marshalCorraldSlices(ch *CorraldHost) (filesJSON, pausedJSON, failedJSON string, err error) {
	files := ch.Files
	if files == nil {
		files = []string{}
	}
	paused := ch.PausedUsers
	if paused == nil {
		paused = []int64{}
	}
	failed := ch.FailedReleases
	if failed == nil {
		failed = []int64{}
	}
	fb, err := json.Marshal(files)
	if err != nil {
		return "", "", "", err
	}
	pb, err := json.Marshal(paused)
	if err != nil {
		return "", "", "", err
	}
	flb, err := json.Marshal(failed)
	if err != nil {
		return "", "", "", err
	}
	return string(fb), string(pb), string(flb), nil
}

// UpsertCorraldHost creates or updates the corrald_hosts row for ch.HostID.
func (s *Store) UpsertCorraldHost(ctx context.Context, ch *CorraldHost) error {
	now := s.unix()
	ch.UpdatedAt = now
	filesJSON, pausedJSON, failedJSON, err := marshalCorraldSlices(ch)
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
			manifest_json, version, release, machine_id, socket_path, files_json, paused_users_json, failed_releases_json,
			active_op_id, updated_from_ver, updated_from_rel, updated_from_target, update_target_ver, update_target_rel,
			update_ready_at, update_crash_count, last_ok_at, last_error, restart_count, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			failed_releases_json = excluded.failed_releases_json,
			active_op_id = excluded.active_op_id,
			updated_from_ver = excluded.updated_from_ver,
			updated_from_rel = excluded.updated_from_rel,
			updated_from_target = excluded.updated_from_target,
			update_target_ver = excluded.update_target_ver,
			update_target_rel = excluded.update_target_rel,
			update_ready_at = excluded.update_ready_at,
			update_crash_count = excluded.update_crash_count,
			last_ok_at = excluded.last_ok_at,
			last_error = excluded.last_error,
			restart_count = excluded.restart_count,
			updated_at = excluded.updated_at`,
		ch.HostID, ch.State, consentBy, ch.ConsentAt,
		ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway,
		ch.ManifestJSON, ch.Version, ch.Release,
		ch.MachineID, ch.SocketPath, filesJSON, pausedJSON, failedJSON,
		ch.ActiveOpID, ch.UpdatedFromVer, ch.UpdatedFromRel, ch.UpdatedFromTarget,
		ch.UpdateTargetVer, ch.UpdateTargetRel, ch.UpdateReadyAt, ch.UpdateCrashCount,
		ch.LastOKAt, ch.LastError, ch.RestartCount, now,
	)
	return err
}

// UpdateCorraldHostIfOp conditionally updates corrald_hosts ONLY if the row's
// current active_op_id matches expectedOpID. This prevents a cancelled or stale
// install/update goroutine from overwriting a withdrawal-first remove state.
// When clearOp is true, active_op_id is cleared to ” on success.
func (s *Store) UpdateCorraldHostIfOp(ctx context.Context, ch *CorraldHost, expectedOpID string, clearOp bool) (bool, error) {
	if expectedOpID == "" {
		return true, s.UpsertCorraldHost(ctx, ch)
	}
	now := s.unix()
	ch.UpdatedAt = now
	nextOpID := expectedOpID
	if clearOp {
		nextOpID = ""
	}
	filesJSON, pausedJSON, failedJSON, err := marshalCorraldSlices(ch)
	if err != nil {
		return false, err
	}
	var consentBy any
	if ch.ConsentBy != nil {
		consentBy = *ch.ConsentBy
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts SET
			state = ?,
			consent_by = ?,
			consent_at = ?,
			bound_fingerprint = ?,
			bound_username = ?,
			bound_gateway = ?,
			manifest_json = ?,
			version = ?,
			release = ?,
			machine_id = ?,
			socket_path = ?,
			files_json = ?,
			paused_users_json = ?,
			failed_releases_json = ?,
			active_op_id = ?,
			updated_from_ver = ?,
			updated_from_rel = ?,
			updated_from_target = ?,
			update_target_ver = ?,
			update_target_rel = ?,
			update_ready_at = ?,
			update_crash_count = ?,
			last_ok_at = ?,
			last_error = ?,
			restart_count = ?,
			updated_at = ?
		WHERE host_id = ? AND active_op_id = ?`,
		ch.State, consentBy, ch.ConsentAt,
		ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway,
		ch.ManifestJSON, ch.Version, ch.Release,
		ch.MachineID, ch.SocketPath, filesJSON, pausedJSON, failedJSON,
		nextOpID, ch.UpdatedFromVer, ch.UpdatedFromRel, ch.UpdatedFromTarget,
		ch.UpdateTargetVer, ch.UpdateTargetRel, ch.UpdateReadyAt, ch.UpdateCrashCount,
		ch.LastOKAt, ch.LastError, ch.RestartCount, now,
		ch.HostID, expectedOpID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		ch.ActiveOpID = nextOpID
		return true, nil
	}
	return false, nil
}

// ClaimCorraldHostOp atomically claims active_op_id = opID on hostID ONLY when
// no other op is currently active and the host is not off/removing/remove_pending.
// When nextState is non-empty it updates state; when manifestJSON is non-empty
// it updates manifest_json.
func (s *Store) ClaimCorraldHostOp(ctx context.Context, hostID int64, opID, nextState, manifestJSON string) (bool, error) {
	now := s.unix()
	var (
		res sql.Result
		err error
	)
	switch {
	case nextState != "" && manifestJSON != "":
		res, err = s.db.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET active_op_id = ?, state = ?, manifest_json = ?, last_error = '', updated_at = ?
			WHERE host_id = ? AND active_op_id = '' AND state NOT IN ('off', 'removing', 'remove_pending')`,
			opID, nextState, manifestJSON, now, hostID)
	case nextState != "":
		res, err = s.db.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET active_op_id = ?, state = ?, last_error = '', updated_at = ?
			WHERE host_id = ? AND active_op_id = '' AND state NOT IN ('off', 'removing', 'remove_pending')`,
			opID, nextState, now, hostID)
	default:
		res, err = s.db.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET active_op_id = ?, updated_at = ?
			WHERE host_id = ? AND active_op_id = '' AND state NOT IN ('off', 'removing', 'remove_pending')`,
			opID, now, hostID)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// FinalizeCorraldOpWithHost atomically updates corrald_hosts (clearing
// active_op_id = ” only if it still matches op.ID) AND updates corrald_ops
// (only if still pending/running) within a single SQLite transaction.
// Prevents a crash between the two writes from leaving a permanent active_op_id.
func (s *Store) FinalizeCorraldOpWithHost(ctx context.Context, op *CorraldOp, ch *CorraldHost) (bool, error) {
	return s.finalizeCorraldOpWithHost(ctx, op, ch, false)
}

// FinalizeCorraldOpErrorWithHost is like FinalizeCorraldOpWithHost, except it
// matches active_op_id IN (”, op.ID) (while still excluding off/removing/
// remove_pending) so error/rollback finalization still succeeds if an earlier
// step (such as attachAndHello encountering proto_mismatch) already cleared
// active_op_id.
func (s *Store) FinalizeCorraldOpErrorWithHost(ctx context.Context, op *CorraldOp, ch *CorraldHost) (bool, error) {
	return s.finalizeCorraldOpWithHost(ctx, op, ch, true)
}

func (s *Store) finalizeCorraldOpWithHost(ctx context.Context, op *CorraldOp, ch *CorraldHost, allowEmptyOp bool) (bool, error) {
	now := s.unix()
	ch.UpdatedAt = now
	op.UpdatedAt = now
	filesJSON, pausedJSON, failedJSON, err := marshalCorraldSlices(ch)
	if err != nil {
		return false, err
	}
	var consentBy any
	if ch.ConsentBy != nil {
		consentBy = *ch.ConsentBy
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	where := `WHERE host_id = ? AND active_op_id = ?`
	if allowEmptyOp {
		where = `WHERE host_id = ? AND active_op_id IN ('', ?) AND state NOT IN ('off', 'removing', 'remove_pending')`
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE corrald_hosts SET
			state = ?,
			consent_by = ?,
			consent_at = ?,
			bound_fingerprint = ?,
			bound_username = ?,
			bound_gateway = ?,
			manifest_json = ?,
			version = ?,
			release = ?,
			machine_id = ?,
			socket_path = ?,
			files_json = ?,
			paused_users_json = ?,
			failed_releases_json = ?,
			active_op_id = '',
			updated_from_ver = ?,
			updated_from_rel = ?,
			updated_from_target = ?,
			update_target_ver = ?,
			update_target_rel = ?,
			update_ready_at = ?,
			update_crash_count = ?,
			last_ok_at = ?,
			last_error = ?,
			restart_count = ?,
			updated_at = ?
		`+where,
		ch.State, consentBy, ch.ConsentAt,
		ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway,
		ch.ManifestJSON, ch.Version, ch.Release,
		ch.MachineID, ch.SocketPath, filesJSON, pausedJSON, failedJSON,
		ch.UpdatedFromVer, ch.UpdatedFromRel, ch.UpdatedFromTarget,
		ch.UpdateTargetVer, ch.UpdateTargetRel, ch.UpdateReadyAt, ch.UpdateCrashCount,
		ch.LastOKAt, ch.LastError, ch.RestartCount, now,
		ch.HostID, op.ID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()

	if _, err := tx.ExecContext(ctx, `
		UPDATE corrald_ops SET state = ?, step = ?, error_code = ?, detail = ?, updated_at = ?
		WHERE id = ? AND state IN ('pending', 'running')`,
		op.State, op.Step, op.ErrorCode, op.Detail, now, op.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if n > 0 {
		ch.ActiveOpID = ""
		return true, nil
	}
	return false, nil
}

// CommitCorraldManifestAndOp atomically commits manifest_json, clears
// active_op_id = ” (when active_op_id still equals op.ID and state is not
// off/removing/remove_pending), and marks op done (or cancelled if superseded)
// in a single transaction.
// CommitCorraldManifestAndOp records consent to the manifest of the daemon
// already running (installed for another host on the same account, or by a
// newer gateway), with that daemon's version and release, and finalizes op.
func (s *Store) CommitCorraldManifestAndOp(ctx context.Context, hostID int64, op *CorraldOp, manifestJSON, version string, release int64) (bool, error) {
	now := s.unix()
	op.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET manifest_json = ?, version = ?, release = ?, active_op_id = '', updated_at = ?
		WHERE host_id = ? AND active_op_id = ? AND state NOT IN ('off', 'removing', 'remove_pending')`,
		manifestJSON, version, release, now, hostID, op.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		op.State = "done"
	} else {
		op.State = "cancelled"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE corrald_ops SET state = ?, step = ?, error_code = ?, detail = ?, updated_at = ?
		WHERE id = ? AND state IN ('pending', 'running')`,
		op.State, op.Step, op.ErrorCode, op.Detail, now, op.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReleaseCorraldHostOpAndFinish atomically clears active_op_id = ” (ONLY if it
// still equals op.ID, optionally restoring updating/installing to fallbackState)
// and finalizes op in a single transaction.
func (s *Store) ReleaseCorraldHostOpAndFinish(ctx context.Context, hostID int64, op *CorraldOp, fallbackState string) (bool, error) {
	if op == nil || op.ID == "" {
		return false, nil
	}
	now := s.unix()
	op.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var (
		res  sql.Result
		qerr error
	)
	if fallbackState != "" {
		res, qerr = tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET active_op_id = '',
			    state = CASE WHEN state IN ('updating', 'installing') THEN ? ELSE state END,
			    updated_at = ?
			WHERE host_id = ? AND active_op_id = ?`,
			fallbackState, now, hostID, op.ID)
	} else {
		res, qerr = tx.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET active_op_id = '', updated_at = ?
			WHERE host_id = ? AND active_op_id = ?`,
			now, hostID, op.ID)
	}
	if qerr != nil {
		return false, qerr
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_ops SET state = ?, step = ?, error_code = ?, detail = ?, updated_at = ?
			WHERE id = ? AND state IN ('pending', 'running')`,
			op.State, op.Step, op.ErrorCode, op.Detail, now, op.ID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClearStaleCorraldHostOps clears active_op_id on all corrald_hosts rows whose
// active_op_id references a finished (done/failed/cancelled/unknown) or missing
// operation in corrald_ops. Called on manager startup for crash recovery.
func (s *Store) ClearStaleCorraldHostOps(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET active_op_id = '', updated_at = ?
		WHERE active_op_id != ''
		  AND NOT EXISTS (
		      SELECT 1 FROM corrald_ops o
		      WHERE o.id = corrald_hosts.active_op_id AND o.state IN ('pending', 'running')
		  )`,
		s.unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearStaleCorraldHostOp clears active_op_id on hostID if it references a
// finished or missing operation in corrald_ops.
func (s *Store) ClearStaleCorraldHostOp(ctx context.Context, hostID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET active_op_id = '', updated_at = ?
		WHERE host_id = ?
		  AND active_op_id != ''
		  AND NOT EXISTS (
		      SELECT 1 FROM corrald_ops o
		      WHERE o.id = corrald_hosts.active_op_id AND o.state IN ('pending', 'running')
		  )`,
		s.unix(), hostID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CommitCorraldManifestIfOp atomically updates ONLY manifest_json and clears
// active_op_id = ” when active_op_id matches opID and the host is not
// off/removing/remove_pending. Prevents scope confirmation from overwriting a
// concurrent withdrawal-first remove.
func (s *Store) CommitCorraldManifestIfOp(ctx context.Context, hostID int64, opID, manifestJSON string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET manifest_json = ?, active_op_id = '', updated_at = ?
		WHERE host_id = ? AND active_op_id = ? AND state NOT IN ('off', 'removing', 'remove_pending')`,
		manifestJSON, s.unix(), hostID, opID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReleaseCorraldHostOp conditionally clears active_op_id = ” on hostID ONLY
// when active_op_id still equals opID. When fallbackState is non-empty and the
// row is still in 'updating' or 'installing', it restores state to fallbackState.
// Safe to call from deferred failure/cancel/panic paths without ever clobbering
// a superseding operation (e.g. withdrawal-first remove).
func (s *Store) ReleaseCorraldHostOp(ctx context.Context, hostID int64, opID, fallbackState string) (bool, error) {
	if opID == "" {
		return false, nil
	}
	now := s.unix()
	var (
		res sql.Result
		err error
	)
	if fallbackState != "" {
		res, err = s.db.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET active_op_id = '',
			    state = CASE WHEN state IN ('updating', 'installing') THEN ? ELSE state END,
			    updated_at = ?
			WHERE host_id = ? AND active_op_id = ?`,
			fallbackState, now, hostID, opID)
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET active_op_id = '', updated_at = ?
			WHERE host_id = ? AND active_op_id = ?`,
			now, hostID, opID)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertCorraldHostIfAbsent inserts ch only if no corrald_hosts row exists for
// ch.HostID yet (used for peer adoption so concurrent installs are never overwritten).
func (s *Store) InsertCorraldHostIfAbsent(ctx context.Context, ch *CorraldHost) (bool, error) {
	now := s.unix()
	ch.UpdatedAt = now
	filesJSON, pausedJSON, failedJSON, err := marshalCorraldSlices(ch)
	if err != nil {
		return false, err
	}
	var consentBy any
	if ch.ConsentBy != nil {
		consentBy = *ch.ConsentBy
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO corrald_hosts (
			host_id, state, consent_by, consent_at, bound_fingerprint, bound_username, bound_gateway,
			manifest_json, version, release, machine_id, socket_path, files_json, paused_users_json, failed_releases_json,
			active_op_id, updated_from_ver, updated_from_rel, updated_from_target, update_target_ver, update_target_rel,
			update_ready_at, update_crash_count, last_ok_at, last_error, restart_count, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ch.HostID, ch.State, consentBy, ch.ConsentAt,
		ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway,
		ch.ManifestJSON, ch.Version, ch.Release,
		ch.MachineID, ch.SocketPath, filesJSON, pausedJSON, failedJSON,
		ch.ActiveOpID, ch.UpdatedFromVer, ch.UpdatedFromRel, ch.UpdatedFromTarget,
		ch.UpdateTargetVer, ch.UpdateTargetRel, ch.UpdateReadyAt, ch.UpdateCrashCount,
		ch.LastOKAt, ch.LastError, ch.RestartCount, now,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimCorraldHostForRemove atomically marks hostID as removing with opID
// without overwriting any other columns from a stale snapshot.
func (s *Store) ClaimCorraldHostForRemove(ctx context.Context, hostID int64, opID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = 'removing', active_op_id = ?, last_error = '', updated_at = ?
		WHERE host_id = ?`,
		opID, s.unix(), hostID)
	return err
}

// ClaimCorraldHostForRetry atomically claims active_op_id = opID for Retry
// only when no other op is active and the host is not off/removing/remove_pending.
func (s *Store) ClaimCorraldHostForRetry(ctx context.Context, hostID int64, opID, nextState string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = ?, restart_count = 0, last_error = '', active_op_id = ?, updated_at = ?
		WHERE host_id = ? AND active_op_id = '' AND state NOT IN ('off', 'removing', 'remove_pending')`,
		nextState, opID, s.unix(), hostID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkCorraldRebindIfMismatch atomically transitions hostID to 'rebind' ONLY
// when:
//  1. The observed identity (observedFP, observedUser) STILL matches the host's
//     current pinned identity in hosts + host_keys (so any stale callback
//     carrying an outdated observed identity A is ignored regardless of whether
//     it read corrald_hosts before or after a newer binding B was written).
//  2. The row's current binding STILL equals the caller's read/expected binding
//     (expectedBoundFP, expectedBoundUser, expectedBoundGW).
//  3. That binding disagrees with the observed identity (observedFP,
//     observedUser, observedGateway).
//  4. The current state is eligible: always excludes 'off', 'removing',
//     'remove_pending', 'rebind'. When includeInstalling is false, also
//     excludes 'installing'; when true, 'installing' rows are only transitioned
//     if a non-empty bound column disagrees with the observed identity.
//
// Returns true iff the row was actually transitioned to 'rebind'.
func (s *Store) MarkCorraldRebindIfMismatch(
	ctx context.Context,
	hostID int64,
	expectedBoundFP, expectedBoundUser, expectedBoundGW string,
	observedFP, observedUser, observedGateway string,
	includeInstalling bool,
) (bool, error) {
	now := s.unix()
	var (
		res sql.Result
		err error
	)
	if includeInstalling {
		res, err = s.db.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'rebind', updated_at = ?
			WHERE host_id = ?
			  AND bound_fingerprint = ? AND bound_username = ? AND bound_gateway = ?
			  AND EXISTS (
			    SELECT 1 FROM hosts h
			    LEFT JOIN host_keys k ON k.host_id = h.id
			    WHERE h.id = corrald_hosts.host_id
			      AND h.username = ?
			      AND COALESCE(k.fingerprint, '') = ?
			  )
			  AND (
			    (state = 'installing' AND (
			      (bound_fingerprint != '' AND bound_fingerprint != ?)
			      OR (bound_username != '' AND bound_username != ?)
			      OR (bound_gateway != '' AND bound_gateway != ?)
			    ))
			    OR
			    (state NOT IN ('off', 'removing', 'remove_pending', 'rebind', 'installing') AND (
			      bound_fingerprint = '' OR bound_username = '' OR bound_gateway = ''
			      OR bound_fingerprint != ? OR bound_username != ? OR bound_gateway != ?
			    ))
			  )`,
			now, hostID,
			expectedBoundFP, expectedBoundUser, expectedBoundGW,
			observedUser, observedFP,
			observedFP, observedUser, observedGateway,
			observedFP, observedUser, observedGateway,
		)
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE corrald_hosts
			SET state = 'rebind', updated_at = ?
			WHERE host_id = ?
			  AND bound_fingerprint = ? AND bound_username = ? AND bound_gateway = ?
			  AND EXISTS (
			    SELECT 1 FROM hosts h
			    LEFT JOIN host_keys k ON k.host_id = h.id
			    WHERE h.id = corrald_hosts.host_id
			      AND h.username = ?
			      AND COALESCE(k.fingerprint, '') = ?
			  )
			  AND state NOT IN ('off', 'removing', 'remove_pending', 'rebind', 'installing')
			  AND (
			    bound_fingerprint = '' OR bound_username = '' OR bound_gateway = ''
			    OR bound_fingerprint != ? OR bound_username != ? OR bound_gateway != ?
			  )`,
			now, hostID,
			expectedBoundFP, expectedBoundUser, expectedBoundGW,
			observedUser, observedFP,
			observedFP, observedUser, observedGateway,
		)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TouchCorraldLastOK updates ONLY last_ok_at while the host is running.
func (s *Store) TouchCorraldLastOK(ctx context.Context, hostID, lastOK int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET last_ok_at = ?, updated_at = ?
		WHERE host_id = ? AND state = 'running'`,
		lastOK, s.unix(), hostID)
	return err
}

// SetCorraldStateIfIdle updates ONLY state when active_op_id = ” and the
// current state is one of allowedFrom.
func (s *Store) SetCorraldStateIfIdle(ctx context.Context, hostID int64, nextState string, allowedFrom ...string) (bool, error) {
	if len(allowedFrom) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(allowedFrom))
	args := make([]any, 0, 3+len(allowedFrom))
	args = append(args, nextState, s.unix(), hostID)
	for i, st := range allowedFrom {
		placeholders[i] = "?"
		args = append(args, st)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE corrald_hosts SET state = ?, updated_at = ?
		 WHERE host_id = ? AND active_op_id = '' AND state IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearCorraldErrorForUpgradeIfIdle resets restart_count and last_error when
// an idle error-state host is eligible for retry on a newer gateway release.
func (s *Store) ClearCorraldErrorForUpgradeIfIdle(ctx context.Context, hostID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET restart_count = 0, last_error = '', updated_at = ?
		WHERE host_id = ? AND active_op_id = '' AND state = 'error'`,
		s.unix(), hostID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetCorraldErrorIfIdle marks an idle non-terminal host as error with lastError.
func (s *Store) SetCorraldErrorIfIdle(ctx context.Context, hostID int64, lastError string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = 'error', last_error = ?, updated_at = ?
		WHERE host_id = ? AND active_op_id = ''
		  AND state NOT IN ('off', 'removing', 'remove_pending', 'rebind', 'installing', 'updating')`,
		lastError, s.unix(), hostID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetCorraldIncompatibleIfIdle marks an idle host as incompatible without
// clobbering concurrent operations or identity rebind.
func (s *Store) SetCorraldIncompatibleIfIdle(ctx context.Context, hostID int64, version string, release int64, lastError string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = 'incompatible',
		    version = CASE WHEN ? != '' THEN ? ELSE version END,
		    release = CASE WHEN ? > 0 THEN ? ELSE release END,
		    last_error = CASE WHEN ? != '' THEN ? ELSE last_error END,
		    updated_at = ?
		WHERE host_id = ? AND active_op_id = ''
		  AND state NOT IN ('off', 'removing', 'remove_pending', 'rebind', 'installing', 'updating')`,
		version, version, release, release, lastError, lastError, s.unix(), hostID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecordCorraldRestartAttemptIfLost updates state, restart_count, and optional
// last_error after a failed automatic restart attempt while the host is still lost.
func (s *Store) RecordCorraldRestartAttemptIfLost(ctx context.Context, hostID int64, nextState string, restartCount int, lastError string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = ?,
		    restart_count = ?,
		    last_error = CASE WHEN ? != '' THEN ? ELSE last_error END,
		    updated_at = ?
		WHERE host_id = ? AND active_op_id = '' AND state = 'lost'`,
		nextState, restartCount, lastError, lastError, s.unix(), hostID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateCorraldRunningIfIdle conditionally updates runtime fields to 'running'
// ONLY when no operation is active, the host is not in a terminal/rebind/op
// state, and its bound identity still matches any non-empty boundFP/boundUser/
// boundGW arguments. Never overwrites paused_users_json, failed_releases_json,
// consent, or manifest_json.
func (s *Store) UpdateCorraldRunningIfIdle(
	ctx context.Context,
	hostID int64,
	boundFP, boundUser, boundGW string,
	version string,
	release int64,
	socketPath, machineID string,
	files []string,
	lastOK int64,
	clearNonUpdateErr bool,
	forceLastError *string,
) (bool, error) {
	filesJSON := ""
	if len(files) > 0 {
		if b, err := json.Marshal(files); err == nil {
			filesJSON = string(b)
		}
	}
	now := s.unix()
	var errExpr string
	args := []any{
		version, version,
		release, release,
		socketPath, socketPath,
		machineID, machineID,
		filesJSON, filesJSON,
		lastOK,
	}
	if forceLastError != nil {
		errExpr = "?"
		args = append(args, *forceLastError)
	} else if clearNonUpdateErr {
		errExpr = `CASE WHEN last_error LIKE '%"code":"update_failed"%' THEN last_error ELSE '' END`
	} else {
		errExpr = "last_error"
	}
	args = append(args, now, hostID)

	where := `WHERE host_id = ? AND active_op_id = '' AND state NOT IN ('off', 'removing', 'remove_pending', 'rebind', 'installing', 'updating')`
	if boundFP != "" {
		where += ` AND bound_fingerprint = ?`
		args = append(args, boundFP)
	}
	if boundUser != "" {
		where += ` AND bound_username = ?`
		args = append(args, boundUser)
	}
	if boundGW != "" {
		where += ` AND bound_gateway = ?`
		args = append(args, boundGW)
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts
		SET state = 'running',
		    version = CASE WHEN ? != '' THEN ? ELSE version END,
		    release = CASE WHEN ? > 0 THEN ? ELSE release END,
		    socket_path = CASE WHEN ? != '' THEN ? ELSE socket_path END,
		    machine_id = CASE WHEN ? != '' THEN ? ELSE machine_id END,
		    files_json = CASE WHEN ? != '' THEN ? ELSE files_json END,
		    restart_count = 0,
		    last_ok_at = ?,
		    last_error = `+errExpr+`,
		    updated_at = ?
		`+where,
		args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateCorraldHostForReconcile atomically updates corrald_hosts (and optional
// activeOp in the same transaction) ONLY if active_op_id is empty, matches
// expectedOpID, or points to an already-finished/nonexistent operation in
// corrald_ops. Always clears active_op_id to empty and never clobbers a
// different live pending/running operation.
func (s *Store) UpdateCorraldHostForReconcile(ctx context.Context, ch *CorraldHost, activeOp *CorraldOp, expectedOpID string) (bool, error) {
	now := s.unix()
	ch.UpdatedAt = now
	filesJSON, pausedJSON, failedJSON, err := marshalCorraldSlices(ch)
	if err != nil {
		return false, err
	}
	var consentBy any
	if ch.ConsentBy != nil {
		consentBy = *ch.ConsentBy
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	where := `WHERE host_id = ? AND (
		active_op_id = ''
		OR NOT EXISTS (
			SELECT 1 FROM corrald_ops o
			WHERE o.id = corrald_hosts.active_op_id AND o.state IN ('pending', 'running')
		)
	)`
	args := []any{
		ch.State, consentBy, ch.ConsentAt,
		ch.BoundFingerprint, ch.BoundUsername, ch.BoundGateway,
		ch.ManifestJSON, ch.Version, ch.Release,
		ch.MachineID, ch.SocketPath, filesJSON, pausedJSON, failedJSON,
		ch.UpdatedFromVer, ch.UpdatedFromRel, ch.UpdatedFromTarget,
		ch.UpdateTargetVer, ch.UpdateTargetRel, ch.UpdateReadyAt, ch.UpdateCrashCount,
		ch.LastOKAt, ch.LastError, ch.RestartCount, now,
		ch.HostID,
	}
	if expectedOpID != "" {
		where = `WHERE host_id = ? AND (
			active_op_id = ''
			OR active_op_id = ?
			OR NOT EXISTS (
				SELECT 1 FROM corrald_ops o
				WHERE o.id = corrald_hosts.active_op_id AND o.state IN ('pending', 'running')
			)
		)`
		args = append(args, expectedOpID)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE corrald_hosts SET
			state = ?,
			consent_by = ?,
			consent_at = ?,
			bound_fingerprint = ?,
			bound_username = ?,
			bound_gateway = ?,
			manifest_json = ?,
			version = ?,
			release = ?,
			machine_id = ?,
			socket_path = ?,
			files_json = ?,
			paused_users_json = ?,
			failed_releases_json = ?,
			active_op_id = '',
			updated_from_ver = ?,
			updated_from_rel = ?,
			updated_from_target = ?,
			update_target_ver = ?,
			update_target_rel = ?,
			update_ready_at = ?,
			update_crash_count = ?,
			last_ok_at = ?,
			last_error = ?,
			restart_count = ?,
			updated_at = ?
		`+where, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if activeOp != nil {
		activeOp.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `
			UPDATE corrald_ops SET state = ?, step = ?, error_code = ?, detail = ?, updated_at = ?
			WHERE id = ? AND state IN ('pending', 'running')`,
			activeOp.State, activeOp.Step, activeOp.ErrorCode, activeOp.Detail, now, activeOp.ID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if n > 0 {
		ch.ActiveOpID = ""
		return true, nil
	}
	return false, nil
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
		    paused_users_json = '[]', failed_releases_json = '[]', active_op_id = '',
		    updated_from_ver = '', updated_from_rel = 0, updated_from_target = '',
		    update_target_ver = '', update_target_rel = 0, update_ready_at = 0, update_crash_count = 0,
		    last_error = '', restart_count = 0, updated_at = ?
		WHERE bound_fingerprint = ? AND bound_username = ?`,
		s.unix(), fingerprint, username)
	return err
}

// RecordCorraldFailedReleases merges releases into failed_releases_json for
// hostID and any peer host sharing (fingerprint, username).
func (s *Store) RecordCorraldFailedReleases(ctx context.Context, hostID int64, fingerprint, username string, releases []int64) error {
	if len(releases) == 0 {
		return nil
	}
	hostIDs := []int64{}
	if hostID > 0 {
		hostIDs = append(hostIDs, hostID)
	}
	if peers, err := s.CorraldPeersByBinding(ctx, fingerprint, username); err == nil {
		for _, p := range peers {
			if !slices.Contains(hostIDs, p.HostID) {
				hostIDs = append(hostIDs, p.HostID)
			}
		}
	}
	now := s.unix()
	for _, id := range hostIDs {
		ch, err := s.CorraldHostByID(ctx, id)
		if err != nil {
			continue
		}
		changed := false
		for _, r := range releases {
			if r > 0 && !slices.Contains(ch.FailedReleases, r) {
				ch.FailedReleases = append(ch.FailedReleases, r)
				changed = true
			}
		}
		if changed {
			b, _ := json.Marshal(ch.FailedReleases)
			_, _ = s.db.ExecContext(ctx,
				`UPDATE corrald_hosts SET failed_releases_json = ?, updated_at = ? WHERE host_id = ?`,
				string(b), now, id)
		}
	}
	return nil
}

// UpdateCorraldCrashWindowByBinding persists post-update crash-loop rollback
// tracking fields for hostID and any peers sharing (fingerprint, username).
func (s *Store) UpdateCorraldCrashWindowByBinding(
	ctx context.Context,
	hostID int64,
	fingerprint, username string,
	readyAt int64,
	targetVer string,
	targetRel int64,
	fromVer string,
	fromRel int64,
	fromTarget string,
	crashCount int,
) error {
	now := s.unix()
	if fingerprint != "" && username != "" {
		_, err := s.db.ExecContext(ctx, `
			UPDATE corrald_hosts SET
				updated_from_ver = ?, updated_from_rel = ?, updated_from_target = ?,
				update_target_ver = ?, update_target_rel = ?, update_ready_at = ?,
				update_crash_count = ?, updated_at = ?
			WHERE (bound_fingerprint = ? AND bound_username = ?) OR host_id = ?`,
			fromVer, fromRel, fromTarget, targetVer, targetRel, readyAt, crashCount, now,
			fingerprint, username, hostID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE corrald_hosts SET
			updated_from_ver = ?, updated_from_rel = ?, updated_from_target = ?,
			update_target_ver = ?, update_target_rel = ?, update_ready_at = ?,
			update_crash_count = ?, updated_at = ?
		WHERE host_id = ?`,
		fromVer, fromRel, fromTarget, targetVer, targetRel, readyAt, crashCount, now, hostID)
	return err
}

// ListUserCorraldHosts returns all hosts owned by userID that have a non-off corrald_hosts row.
func (s *Store) ListUserCorraldHosts(ctx context.Context, userID int64) ([]*Host, []*CorraldHost, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.owner_id, h.name, h.address, h.port, h.username, h.auth, h.credential_id,
		       h.persistence, h.tmux_session, h.herdr_session, h.created_at, h.updated_at, h.last_connected_at,
		       h.conn_version, h.version, h.jump_host_id, `+corraldHostQualifiedCols+`
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
		var filesJSON, pausedJSON, failedJSON string
		if err := rows.Scan(
			&h.ID, &h.OwnerID, &h.Name, &h.Address, &h.Port, &h.Username, &h.Auth, &cred,
			&h.Persistence, &h.TmuxSession, &h.HerdrSession, &h.CreatedAt, &h.UpdatedAt, &h.LastConnectedAt,
			&h.ConnVersion, &h.Version, &jump,
			&ch.HostID, &ch.State, &consentBy, &ch.ConsentAt,
			&ch.BoundFingerprint, &ch.BoundUsername, &ch.BoundGateway,
			&ch.ManifestJSON, &ch.Version, &ch.Release,
			&ch.MachineID, &ch.SocketPath, &filesJSON, &pausedJSON, &failedJSON,
			&ch.ActiveOpID, &ch.UpdatedFromVer, &ch.UpdatedFromRel, &ch.UpdatedFromTarget,
			&ch.UpdateTargetVer, &ch.UpdateTargetRel, &ch.UpdateReadyAt, &ch.UpdateCrashCount,
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
		if failedJSON != "" {
			_ = json.Unmarshal([]byte(failedJSON), &ch.FailedReleases)
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
		SELECT id, host_id, account_key, kind, state, step, error_code, detail, created_by, created_at, updated_at
		FROM corrald_ops WHERE state IN ('pending', 'running') ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CorraldOp
	for rows.Next() {
		op := &CorraldOp{}
		if err := rows.Scan(&op.ID, &op.HostID, &op.AccountKey, &op.Kind, &op.State, &op.Step, &op.ErrorCode, &op.Detail, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// CreateCorraldOp inserts a new operation. If another pending/running operation
// already exists for op.HostID or op.AccountKey, ErrOpInProgress is returned.
// When supersedeInstallUpdate is true (for withdrawal-first remove), any active
// install or update op for the host OR account_key is cancelled atomically in
// the same transaction and active_op_id on matching corrald_hosts rows is set
// to op.ID so superseded goroutines cannot overwrite removal state.
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
		if op.AccountKey != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE corrald_ops
				SET state = 'cancelled', error_code = 'removing', updated_at = ?
				WHERE (host_id = ? OR account_key = ?) AND state IN ('pending', 'running') AND kind IN ('install', 'update')`,
				now, op.HostID, op.AccountKey); err != nil {
				return err
			}
			// Claim active_op_id on peer rows sharing this account too so stale ops cannot overwrite them.
			fp, user, ok := strings.Cut(op.AccountKey, "\x00")
			if ok && fp != "" && user != "" {
				if _, err := tx.ExecContext(ctx, `
					UPDATE corrald_hosts
					SET active_op_id = ?, updated_at = ?
					WHERE (bound_fingerprint = ? AND bound_username = ?) OR host_id = ?`,
					op.ID, now, fp, user, op.HostID); err != nil {
					return err
				}
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE corrald_ops
				SET state = 'cancelled', error_code = 'removing', updated_at = ?
				WHERE host_id = ? AND state IN ('pending', 'running') AND kind IN ('install', 'update')`,
				now, op.HostID); err != nil {
				return err
			}
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO corrald_ops (id, host_id, account_key, kind, state, step, error_code, detail, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.HostID, op.AccountKey, op.Kind, op.State, op.Step, op.ErrorCode, op.Detail, op.CreatedBy, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrOpInProgress
		}
		return err
	}
	return tx.Commit()
}

// CancelActiveCorraldOps cancels active install/update operations for hostID
// (and accountKey if non-empty) and clears active_op_id on corrald_hosts so
// cancelled goroutines cannot write state back after host deletion.
func (s *Store) CancelActiveCorraldOps(ctx context.Context, hostID int64, accountKey, errorCode string) error {
	now := s.unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if accountKey != "" {
		_, _ = tx.ExecContext(ctx, `
			UPDATE corrald_ops
			SET state = 'cancelled', error_code = ?, updated_at = ?
			WHERE (host_id = ? OR account_key = ?) AND state IN ('pending', 'running') AND kind IN ('install', 'update')`,
			errorCode, now, hostID, accountKey)
	} else {
		_, _ = tx.ExecContext(ctx, `
			UPDATE corrald_ops
			SET state = 'cancelled', error_code = ?, updated_at = ?
			WHERE host_id = ? AND state IN ('pending', 'running') AND kind IN ('install', 'update')`,
			errorCode, now, hostID)
	}
	_, _ = tx.ExecContext(ctx, `UPDATE corrald_hosts SET active_op_id = '', updated_at = ? WHERE host_id = ?`, now, hostID)
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

// UpdateCorraldOpIfActive updates an operation ONLY if it is still pending or
// running, so an already-superseded operation cannot overwrite its cancelled row.
func (s *Store) UpdateCorraldOpIfActive(ctx context.Context, op *CorraldOp) (bool, error) {
	op.UpdatedAt = s.unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE corrald_ops SET state = ?, step = ?, error_code = ?, detail = ?, updated_at = ?
		WHERE id = ? AND state IN ('pending', 'running')`,
		op.State, op.Step, op.ErrorCode, op.Detail, op.UpdatedAt, op.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// LatestCorraldOp returns the most recent operation for hostID, or ErrNotFound.
func (s *Store) LatestCorraldOp(ctx context.Context, hostID int64) (*CorraldOp, error) {
	op := &CorraldOp{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, host_id, account_key, kind, state, step, error_code, detail, created_by, created_at, updated_at
		FROM corrald_ops WHERE host_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, hostID).
		Scan(&op.ID, &op.HostID, &op.AccountKey, &op.Kind, &op.State, &op.Step, &op.ErrorCode, &op.Detail, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return op, nil
}

// ActiveCorraldOp returns the currently pending/running operation for hostID, or ErrNotFound.
func (s *Store) ActiveCorraldOp(ctx context.Context, hostID int64) (*CorraldOp, error) {
	op := &CorraldOp{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, host_id, account_key, kind, state, step, error_code, detail, created_by, created_at, updated_at
		FROM corrald_ops WHERE host_id = ? AND state IN ('pending', 'running') LIMIT 1`, hostID).
		Scan(&op.ID, &op.HostID, &op.AccountKey, &op.Kind, &op.State, &op.Step, &op.ErrorCode, &op.Detail, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return op, nil
}

// SetCorraldUserPaused adds or removes userID in paused_users_json for hostID
// using a targeted column update inside a transaction so concurrent state/op
// transitions on the row are never overwritten.
func (s *Store) SetCorraldUserPaused(ctx context.Context, hostID, userID int64, paused bool) (*CorraldHost, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ch, err := scanCorraldHost(tx.QueryRowContext(ctx,
		`SELECT `+corraldHostCols+` FROM corrald_hosts WHERE host_id = ?`, hostID))
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
	pausedJSON, err := json.Marshal(ch.PausedUsers)
	if err != nil {
		return nil, err
	}
	now := s.unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE corrald_hosts SET paused_users_json = ?, updated_at = ? WHERE host_id = ?`,
		string(pausedJSON), now, hostID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ch.UpdatedAt = now
	return ch, nil
}
