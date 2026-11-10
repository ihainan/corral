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
	Mode      string
	TmuxName  string
	Pane      string
	Socket    string
	PID       int64
	Start     int64
	SID       string
	Log       string
	LogOff    int64
	CreatedAt int64
}

// PutSettingsPending keeps p, in place of the account's earlier one (its
// time kept where p has one: the same switch, known better).
func (s *Store) PutSettingsPending(ctx context.Context, p SettingsPending) error {
	at := p.CreatedAt
	if at == 0 {
		at = s.unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO settings_pending
		(account, owner_id, host_id, had, was, wrote, mode, tmux_name, pane, socket, pid, start, sid, log, log_off, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Account, p.OwnerID, p.HostID, p.Had, nonNil(p.Was), nonNil(p.Wrote), p.Mode, p.TmuxName, p.Pane, p.Socket, p.PID, p.Start, p.SID, p.Log, p.LogOff, at)
	return err
}

// SettingsPendingOf gives the account's, or nil when it has none.
func (s *Store) SettingsPendingOf(ctx context.Context, account string) (*SettingsPending, error) {
	p := SettingsPending{Account: account}
	err := s.db.QueryRowContext(ctx, `SELECT owner_id, host_id, had, was, wrote, mode, tmux_name, pane, socket, pid, start, sid, log, log_off, created_at
		FROM settings_pending WHERE account = ?`, account).
		Scan(&p.OwnerID, &p.HostID, &p.Had, &p.Was, &p.Wrote, &p.Mode, &p.TmuxName, &p.Pane, &p.Socket, &p.PID, &p.Start, &p.SID, &p.Log, &p.LogOff, &p.CreatedAt)
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
