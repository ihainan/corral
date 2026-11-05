package store

import "context"

type Recording struct {
	ID        string `json:"id"`
	OwnerID   int64  `json:"owner_id"`
	OwnerName string `json:"owner_name"`
	HostID    int64  `json:"host_id"`
	HostName  string `json:"host_name"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at"` // 0 while recording
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Gaps      bool   `json:"gaps"`
}

const recordingCols = `id, owner_id, owner_name, host_id, host_name, started_at, ended_at, bytes, truncated, gaps`

func scanRecording(row interface{ Scan(...any) error }) (*Recording, error) {
	r := &Recording{}
	err := row.Scan(&r.ID, &r.OwnerID, &r.OwnerName, &r.HostID, &r.HostName, &r.StartedAt, &r.EndedAt, &r.Bytes, &r.Truncated, &r.Gaps)
	if err != nil {
		return nil, notFound(err)
	}
	return r, nil
}

func (s *Store) CreateRecording(ctx context.Context, r *Recording) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO recordings (`+recordingCols+`) VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0)`,
		r.ID, r.OwnerID, r.OwnerName, r.HostID, r.HostName, r.StartedAt)
	return err
}

func (s *Store) FinishRecording(ctx context.Context, id string, bytes int64, truncated, gaps bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET ended_at = ?, bytes = ?, truncated = ?, gaps = ? WHERE id = ?`,
		s.unix(), bytes, truncated, gaps, id)
	return err
}

func (s *Store) RecordingByID(ctx context.Context, id string) (*Recording, error) {
	return scanRecording(s.db.QueryRowContext(ctx, `SELECT `+recordingCols+` FROM recordings WHERE id = ?`, id))
}

// RecordingFilter narrows a list of recordings: one owner's (0: everyone's),
// one host's (0: any; by id, so a deleted host's still match), started in
// [From, To) (0: no bound).
type RecordingFilter struct {
	Owner, Host int64
	From, To    int64
}

// ListRecordings returns recordings in order newest first (and by id among
// those started the same second), after the one at (before, afterID) when
// before is set, that match f.
func (s *Store) ListRecordings(ctx context.Context, f RecordingFilter, before int64, afterID string, limit int) ([]*Recording, error) {
	q, args := `SELECT `+recordingCols+` FROM recordings WHERE 1 = 1`, []any{}
	if f.Owner != 0 {
		q, args = q+` AND owner_id = ?`, append(args, f.Owner)
	}
	if f.Host != 0 {
		q, args = q+` AND host_id = ?`, append(args, f.Host)
	}
	if f.From != 0 {
		q, args = q+` AND started_at >= ?`, append(args, f.From)
	}
	if f.To != 0 {
		q, args = q+` AND started_at < ?`, append(args, f.To)
	}
	if before != 0 {
		q, args = q+` AND (started_at < ? OR (started_at = ? AND id > ?))`, append(args, before, before, afterID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY started_at DESC, id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Recording{}
	for rows.Next() {
		r, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordingChoice is one host, or one owner, there are recordings of: what a
// list of them can be narrowed to (by id; the name as last recorded).
type RecordingChoice struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Of a host: whose (as last recorded) — two hosts of one name told apart.
	Owner string `json:"owner,omitempty"`
}

// RecordingChoices returns the hosts and the owners of the recordings owner
// may see (0: all), the most recently recorded first.
func (s *Store) RecordingChoices(ctx context.Context, owner int64) (hosts, owners []RecordingChoice, err error) {
	// (SQLite gives the bare name column of the row MAX picked: the name as
	// last recorded.)
	pick := func(id, name string) ([]RecordingChoice, error) {
		q, args := `SELECT `+id+`, `+name+`, owner_name, MAX(started_at) FROM recordings`, []any{}
		if owner != 0 {
			q, args = q+` WHERE owner_id = ?`, append(args, owner)
		}
		// (All of them: as many as there are hosts and people recorded, a list
		// cut short would leave some out of reach — review cd07bb9.)
		rows, err := s.db.QueryContext(ctx, q+` GROUP BY `+id+` ORDER BY MAX(started_at) DESC`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []RecordingChoice{}
		for rows.Next() {
			var c RecordingChoice
			var at int64
			var owner string
			if err := rows.Scan(&c.ID, &c.Name, &owner, &at); err != nil {
				return nil, err
			}
			if id == "host_id" {
				c.Owner = owner
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}
	if hosts, err = pick("host_id", "host_name"); err != nil {
		return nil, nil, err
	}
	if owners, err = pick("owner_id", "owner_name"); err != nil {
		return nil, nil, err
	}
	return hosts, owners, nil
}

// HasRecordings reports whether there is a recording owner may see (owner 0:
// any recording).
func (s *Store) HasRecordings(ctx context.Context, owner int64) (bool, error) {
	q, args := `SELECT EXISTS (SELECT 1 FROM recordings)`, []any{}
	if owner != 0 {
		q, args = `SELECT EXISTS (SELECT 1 FROM recordings WHERE owner_id = ?)`, []any{owner}
	}
	var ok bool
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&ok)
	return ok, err
}

func (s *Store) DeleteRecording(ctx context.Context, id string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, id))
}

// FinishStaleRecording marks a recording that was left open as ended (cut
// short, size unknown); one that ended meanwhile keeps its own result.
func (s *Store) FinishStaleRecording(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET ended_at = ?, bytes = -1, truncated = 1 WHERE id = ? AND ended_at = 0`, s.unix(), id)
	return err
}

// RecordingsBefore lists finished recordings started before t, for pruning.
func (s *Store) RecordingsBefore(ctx context.Context, t int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recordings WHERE started_at < ? AND ended_at != 0`, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UnfinishedRecordings lists recordings left open (the gateway stopped while
// they were being made), so they can be closed on start-up.
func (s *Store) UnfinishedRecordings(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recordings WHERE ended_at = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetRecordingStart changes when a recording counts as started (tests of
// the retention period).
func (s *Store) SetRecordingStart(ctx context.Context, id string, at int64) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `UPDATE recordings SET started_at = ? WHERE id = ?`, at, id))
}
