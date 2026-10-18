package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ihainan/corral/internal/store"
)

func newTestNotifier(t *testing.T, opt Options) (*Notifier, *store.Store) {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n, err := New(context.Background(), st, slog.New(slog.NewTextHandler(io.Discard, nil)), opt)
	if err != nil {
		t.Fatal(err)
	}
	return n, st
}

func TestWebhookPrefixesMatchWholeHostAndSegments(t *testing.T) {
	n, _ := newTestNotifier(t, Options{WebhookPrefixes: []string{"https://hooks.example.com", "https://api.example.com:8443/hooks/team"}})
	for _, ok := range []string{
		"https://hooks.example.com", "https://hooks.example.com/", "https://HOOKS.example.com/any/path?x=1",
		"https://api.example.com:8443/hooks/team", "https://api.example.com:8443/hooks/team/", "https://api.example.com:8443/hooks/team/abc",
	} {
		if err := n.CheckWebhook(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"https://hooks.example.com.attacker.example/", // the raw prefix is a string prefix of this host
		"https://hooks.example.com@attacker.example/",
		"https://user@hooks.example.com/",
		"http://hooks.example.com/",
		"https://hooks.example.com:444/",
		"https://api.example.com/hooks/team/abc", // port differs
		"https://api.example.com:8443/hooks/teammate",
		"https://api.example.com:8443/hooks/team/../../admin",
		"https://api.example.com:8443/hooks/team/%2e%2e/admin",
		"https://api.example.com:8443/hooks/team%2F..%2Fadmin",
		"https://api.example.com:8443/hooks",
		"hooks.example.com/x",
	} {
		if err := n.CheckWebhook(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestAmbiguousWebhookPrefixesAreRejected(t *testing.T) {
	for _, p := range []string{"hooks.example.com", "https://", "ftp://hooks.example.com/", "https://hooks.example.com/x?y=1",
		"https://hooks.example.com/#frag", "https://u:p@hooks.example.com/", "https://hooks.example.com/a/../b"} {
		st, _ := store.OpenMemory()
		_, err := New(context.Background(), st, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{WebhookPrefixes: []string{p}})
		st.Close()
		if err == nil {
			t.Errorf("prefix %q accepted", p)
		}
	}
}

func testSubscription(t *testing.T, endpoint string) *store.PushSubscription {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	return &store.PushSubscription{Endpoint: endpoint, P256dh: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		Auth: base64.RawURLEncoding.EncodeToString(secret)}
}

func TestPushDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			elsewhere.Add(1)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	n, _ := newTestNotifier(t, Options{ExtraPushHosts: []string{"127.0.0.1"}})
	n.http.Transport = srv.Client().Transport // trust the test certificate; the redirect policy stays
	res, err := n.push(context.Background(), testSubscription(t, srv.URL+"/push"), []byte(`{}`), Note{})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTemporaryRedirect || elsewhere.Load() != 0 {
		t.Fatalf("push followed a redirect: status %d, %d hits elsewhere", res.StatusCode, elsewhere.Load())
	}
}
