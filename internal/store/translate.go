package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// TranslateConfig holds a user's thinking-translation settings.
// The API key is vault-sealed in Ciphertext/WrappedKey/KeyID.
type TranslateConfig struct {
	UserID     int64
	Enabled    bool
	TargetLang string
	BaseURL    string
	Model      string
	Ciphertext []byte
	WrappedKey []byte
	KeyID      string
	UpdatedAt  int64
}

const (
	DefaultTranslateLang    = "zh-CN"
	DefaultTranslateBaseURL = "https://api.deepseek.com/v1"
	DefaultTranslateModel   = "deepseek-flash"
	maxCachedTranslations   = 5000
)

// TranslateConfigOf returns the user's saved thinking-translation config, or
// defaults (enabled=false, no key) if none has been saved yet.
func (s *Store) TranslateConfigOf(ctx context.Context, userID int64) (*TranslateConfig, error) {
	c := &TranslateConfig{
		UserID:     userID,
		TargetLang: DefaultTranslateLang,
		BaseURL:    DefaultTranslateBaseURL,
		Model:      DefaultTranslateModel,
	}
	var enabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled, target_lang, base_url, model, ciphertext, wrapped_key, key_id, updated_at
		 FROM user_translate_config WHERE user_id = ?`, userID).
		Scan(&enabled, &c.TargetLang, &c.BaseURL, &c.Model, &c.Ciphertext, &c.WrappedKey, &c.KeyID, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	c.Enabled = enabled != 0
	return c, nil
}

// SaveTranslateConfig upserts the user's thinking-translation config.
func (s *Store) SaveTranslateConfig(ctx context.Context, c *TranslateConfig) error {
	now := time.Now().Unix()
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_translate_config (user_id, enabled, target_lang, base_url, model, ciphertext, wrapped_key, key_id, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   enabled = excluded.enabled,
		   target_lang = excluded.target_lang,
		   base_url = excluded.base_url,
		   model = excluded.model,
		   ciphertext = excluded.ciphertext,
		   wrapped_key = excluded.wrapped_key,
		   key_id = excluded.key_id,
		   updated_at = excluded.updated_at`,
		c.UserID, enabled, c.TargetLang, c.BaseURL, c.Model, c.Ciphertext, c.WrappedKey, c.KeyID, now)
	return err
}

// CachedThoughtTranslations fetches any cached translations for userID and
// targetLang among hashes, returning a map of text_hash -> translated text.
func (s *Store) CachedThoughtTranslations(ctx context.Context, userID int64, targetLang string, hashes []string) (map[string]string, error) {
	out := map[string]string{}
	if len(hashes) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(hashes)+2)
	args = append(args, userID, targetLang)
	ph := make([]string, len(hashes))
	for i, h := range hashes {
		ph[i] = "?"
		args = append(args, h)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT text_hash, translated FROM thought_translations
		 WHERE user_id = ? AND target_lang = ? AND text_hash IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h, txt string
		if err := rows.Scan(&h, &txt); err != nil {
			return nil, err
		}
		out[h] = txt
	}
	return out, rows.Err()
}

// PutThoughtTranslation saves a translated thinking block and prunes old entries
// beyond maxCachedTranslations for this user.
func (s *Store) PutThoughtTranslation(ctx context.Context, userID int64, textHash, targetLang, translated string) error {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO thought_translations (user_id, text_hash, target_lang, translated, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, text_hash, target_lang) DO UPDATE SET
		   translated = excluded.translated,
		   updated_at = excluded.updated_at`,
		userID, textHash, targetLang, translated, now); err != nil {
		return err
	}
	// Keep the cache bounded per user (prune only when over the cap).
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM thought_translations
		 WHERE user_id = ? AND rowid IN (
		   SELECT rowid FROM thought_translations WHERE user_id = ?
		   ORDER BY updated_at DESC, rowid DESC LIMIT -1 OFFSET ?
		 )`, userID, userID, maxCachedTranslations)
	return nil
}
