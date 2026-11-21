package store_test

import (
	"context"
	"testing"

	"github.com/ihainan/corral/internal/store"
)

func TestTranslateStoreConfigAndCache(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u1, err := st.CreateUser(ctx, "alice", "Alice", "hash1", "admin")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := st.CreateUser(ctx, "bob", "Bob", "hash2", "member")
	if err != nil {
		t.Fatal(err)
	}

	// Default config when nothing saved yet.
	c, err := st.TranslateConfigOf(ctx, u1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Enabled || c.TargetLang != store.DefaultTranslateLang || c.Model != store.DefaultTranslateModel || len(c.Ciphertext) != 0 {
		t.Fatalf("default config = %+v", c)
	}

	// Save and reload config.
	c.Enabled = true
	c.TargetLang = "ja"
	c.BaseURL = "https://example.test/v1"
	c.Model = "deepseek-flash"
	c.Ciphertext = []byte("ct")
	c.WrappedKey = []byte("wk")
	c.KeyID = "k1"
	if err := st.SaveTranslateConfig(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := st.TranslateConfigOf(ctx, u1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.TargetLang != "ja" || got.BaseURL != "https://example.test/v1" || string(got.Ciphertext) != "ct" {
		t.Fatalf("saved config = %+v", got)
	}

	// Cache isolation between users and languages.
	if err := st.PutThoughtTranslation(ctx, u1.ID, "h1", "zh-CN", "已翻译"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutThoughtTranslation(ctx, u1.ID, "h1", "ja", "翻訳済み"); err != nil {
		t.Fatal(err)
	}
	m1, err := st.CachedThoughtTranslations(ctx, u1.ID, "zh-CN", []string{"h1", "h2"})
	if err != nil || len(m1) != 1 || m1["h1"] != "已翻译" {
		t.Fatalf("alice zh-CN cache = %v err=%v", m1, err)
	}
	m2, err := st.CachedThoughtTranslations(ctx, u2.ID, "zh-CN", []string{"h1"})
	if err != nil || len(m2) != 0 {
		t.Fatalf("bob cache should be isolated, got %v err=%v", m2, err)
	}
}
