package store

import (
	"context"
	"encoding/json"
	"strings"
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
	UserID *int64
	// SignInAs: with UserID, also the sign-ins tried as this user name that
	// failed (or were locked): no one's yet, with no id — the name tried is
	// their target (review cd07bb9).
	SignInAs string
	// Heads: actions whose first part ("login" of "login.failed") is one of
	// these; NotHeads: none of these (what no kind takes in).
	Heads, NotHeads []string
	From, To        int64 // [From, To), unix seconds; 0: no bound
	BeforeID        int64
	Limit           int
}

// (An action's first part: the whole of one with no dot.)
const auditHead = `substr(action, 1, instr(action || '.', '.') - 1)`

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]*AuditEntry, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	sqlq := `SELECT id, ts, user_id, username, action, target, ip, detail FROM audit_log WHERE 1 = 1`
	var args []any
	if q.UserID != nil {
		if q.SignInAs != "" {
			// (As sign-in finds the name: its spaces round it trimmed, its case not minded.)
			sqlq += ` AND (user_id = ? OR (user_id IS NULL AND trim(target, ' ' || char(9) || char(10) || char(13)) = ? COLLATE NOCASE AND action LIKE 'login.%'))`
			args = append(args, *q.UserID, q.SignInAs)
		} else {
			sqlq += ` AND user_id = ?`
			args = append(args, *q.UserID)
		}
	}
	if len(q.Heads) > 0 {
		sqlq += ` AND ` + auditHead + ` IN (` + placeholders(len(q.Heads)) + `)`
		for _, h := range q.Heads {
			args = append(args, h)
		}
	}
	if len(q.NotHeads) > 0 {
		sqlq += ` AND ` + auditHead + ` NOT IN (` + placeholders(len(q.NotHeads)) + `)`
		for _, h := range q.NotHeads {
			args = append(args, h)
		}
	}
	if q.From > 0 {
		sqlq += ` AND ts >= ?`
		args = append(args, q.From)
	}
	if q.To > 0 {
		sqlq += ` AND ts < ?`
		args = append(args, q.To)
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
