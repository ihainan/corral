package store

import (
	"context"
	"database/sql"
	"errors"
)

// PutModelList keeps a picker's list under key (see
// migrations/0019_model_lists.sql), in place of an earlier one.
func (s *Store) PutModelList(ctx context.Context, key string, at int64, rows []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO model_lists (key, at, rows) VALUES (?, ?, ?)`, key, at, nonNil(rows))
	return err
}

// ModelList is the list kept under key (found false: none).
func (s *Store) ModelList(ctx context.Context, key string) (at int64, rows []byte, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT at, rows FROM model_lists WHERE key = ?`, key).Scan(&at, &rows)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, nil
	}
	return at, rows, err == nil, err
}

// DeleteModelList lets the list under key go.
func (s *Store) DeleteModelList(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_lists WHERE key = ?`, key)
	return err
}
