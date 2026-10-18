package store

import (
	"context"
	"encoding/json"
)

type AuditEntry struct {
	ID       int64           `json:"id"`
	TS       int64           `json:"ts"`
	UserID   *int64          `json:"user_id"`
	Username string          `json:"username"`
	Action   string          `json:"action"`
	Target   string          `json:"target"`
	IP       string          `json:"ip"`
	Detail   json.RawMessage `json:"detail"`
}

// Audit appends one entry. Detail must never contain secrets; callers pass
// metadata only (ids, sizes, durations).
func (s *Store) Audit(ctx context.Context, userID *int64, username, action, target, ip string, detail map[string]any) error {
	d := ""
	if len(detail) > 0 {
		b, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		d = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, user_id, username, action, target, ip, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.unix(), userID, username, action, target, ip, d)
	return err
}

type AuditQuery struct {
	UserID   *int64
	BeforeID int64
	Limit    int
}

func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]*AuditEntry, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	sqlq := `SELECT id, ts, user_id, username, action, target, ip, detail FROM audit_log WHERE 1 = 1`
	var args []any
	if q.UserID != nil {
		sqlq += ` AND user_id = ?`
		args = append(args, *q.UserID)
	}
	if q.BeforeID > 0 {
		sqlq += ` AND id < ?`
		args = append(args, q.BeforeID)
	}
	sqlq += ` ORDER BY id DESC LIMIT ?`
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		e := &AuditEntry{}
		var detail string
		if err := rows.Scan(&e.ID, &e.TS, &e.UserID, &e.Username, &e.Action, &e.Target, &e.IP, &detail); err != nil {
			return nil, err
		}
		if detail == "" {
			detail = "{}"
		}
		e.Detail = json.RawMessage(detail)
		out = append(out, e)
	}
	return out, rows.Err()
}
