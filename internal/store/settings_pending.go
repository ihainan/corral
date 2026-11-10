package store

import (
	"context"
	"database/sql"
	"errors"
)

// SettingsPending is a default of Claude Code's settings to be put back:
// see migrations/0018_settings_pending.sql.
type SettingsPending struct {
	Account   string
	OwnerID   int64
	HostID    int64
	Had       bool
	Was       []byte
	Wrote     []byte
	CreatedAt int64
}

// PutSettingsPending keeps p, in place of the account's earlier one.
func (s *Store) PutSettingsPending(ctx context.Context, p SettingsPending) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings_pending (account, owner_id, host_id, had, was, wrote, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account) DO UPDATE SET owner_id = excluded.owner_id, host_id = excluded.host_id, had = excluded.had, was = excluded.was, wrote = excluded.wrote, created_at = excluded.created_at`,
		p.Account, p.OwnerID, p.HostID, p.Had, nonNil(p.Was), nonNil(p.Wrote), s.unix())
	return err
}

// SettingsPendingOf gives the account's, or nil when it has none.
func (s *Store) SettingsPendingOf(ctx context.Context, account string) (*SettingsPending, error) {
	p := SettingsPending{Account: account}
	err := s.db.QueryRowContext(ctx, `SELECT owner_id, host_id, had, was, wrote, created_at FROM settings_pending WHERE account = ?`, account).
		Scan(&p.OwnerID, &p.HostID, &p.Had, &p.Was, &p.Wrote, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DeleteSettingsPending lets the account's go.
func (s *Store) DeleteSettingsPending(ctx context.Context, account string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings_pending WHERE account = ?`, account)
	return err
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
