package store

import "context"

type CodexCommand struct {
	Digest string
	Status int
	Result string
}

func (s *Store) BeginCodexCommand(ctx context.Context, owner, host int64, key, digest string) (CodexCommand, bool, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO codex_commands(owner_id,host_id,command_key,digest,created_at) SELECT ?,id,?,?,? FROM hosts WHERE id=? AND owner_id=? ON CONFLICT DO NOTHING`, owner, key, digest, s.unix(), host, owner)
	if err != nil {
		return CodexCommand{}, false, err
	}
	n, _ := r.RowsAffected()
	var c CodexCommand
	err = s.db.QueryRowContext(ctx, `SELECT digest,status,result FROM codex_commands WHERE owner_id=? AND host_id=? AND command_key=?`, owner, host, key).Scan(&c.Digest, &c.Status, &c.Result)
	return c, n == 1, notFound(err)
}

func (s *Store) FinishCodexCommand(ctx context.Context, owner, host int64, key string, status int, result string) error {
	return affectedOrNotFound(s.db.ExecContext(ctx, `UPDATE codex_commands SET status=?,result=? WHERE owner_id=? AND host_id=? AND command_key=?`, status, result, owner, host, key))
}
