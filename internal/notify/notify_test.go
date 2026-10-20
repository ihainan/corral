package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"strings"
	"sync"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// stallTransport holds every request until its context ends.
type stallTransport struct{ entered chan string }

func (s stallTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.entered <- r.URL.Path
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case <-time.After(5 * time.Second):
		return nil, errors.New("push was not cancelled")
	}
}

// A push still being prepared (DNS, TLS, request) when its login ends is
// cancelled, and only that login's.
func TestRevokingALoginCancelsItsPushesInFlight(t *testing.T) {
	cases := []struct {
		name   string
		revoke func(n *Notifier, userID int64)
	}{
		{"sign-out", func(n *Notifier, id int64) { n.RevokeLogin(id, "phone") }},
		{"password change elsewhere", func(n *Notifier, id int64) { n.RevokeOtherLogins(id, "laptop") }},
		{"user disabled", func(n *Notifier, id int64) { n.Revoke(id) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, st := newTestNotifier(t, Options{ExtraPushHosts: []string{"push.test"},
				LoginValid: func(context.Context, string) (bool, error) { return true, nil }})
			tr := stallTransport{entered: make(chan string, 1)}
			n.http.Transport = tr
			ctx := context.Background()
			u, _ := st.CreateUser(ctx, "alice", "", "x", store.RoleMember)
			if err := st.CreateLoginSession(ctx, "phone", u.ID, "", ""); err != nil {
				t.Fatal(err)
			}
			s := testSubscription(t, "https://push.test/phone")
			s.UserID, s.LoginHash = u.ID, "phone"
			if _, err := st.SavePushSubscription(ctx, s, 10); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { n.deliver(ctx, u.ID, 0, Note{Title: "t"}); close(done) }()
			<-tr.entered
			c.revoke(n, u.ID)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("push in flight was not cancelled")
			}
		})
	}
}

func TestDeliverySkipsEndedLoginsAndRevokedUsers(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	live := map[string]bool{"live": true}
	n, st := newTestNotifier(t, Options{ExtraPushHosts: []string{"127.0.0.1"},
		LoginValid: func(_ context.Context, h string) (bool, error) { return live[h], nil }})
	n.http.Transport = srv.Client().Transport
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice", "", "x", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"live", "stale"} {
		if err := st.CreateLoginSession(ctx, h, u.ID, "", ""); err != nil {
			t.Fatal(err)
		}
		s := testSubscription(t, srv.URL+"/"+h)
		s.UserID, s.LoginHash = u.ID, h
		if _, err := st.SavePushSubscription(ctx, s, 10); err != nil {
			t.Fatal(err)
		}
	}
	// The "stale" login still has its row (not yet pruned) but is no longer valid.
	n.deliver(ctx, u.ID, 0, Note{Title: "t"})
	subs, _ := st.PushSubscriptions(ctx, u.ID)
	if hits.Load() != 1 || len(subs) != 1 || subs[0].LoginHash != "live" {
		t.Fatalf("%d pushes sent, %d subscriptions left", hits.Load(), len(subs))
	}
	// A note queued before the user's access ended is dropped.
	n.Revoke(u.ID)
	n.deliver(ctx, u.ID, 0, Note{Title: "t"})
	if hits.Load() != 1 {
		t.Fatal("a note queued before revocation was still sent")
	}
	// Subscriptions need a login that exists.
	s := testSubscription(t, srv.URL+"/x")
	s.UserID, s.LoginHash = u.ID, "missing"
	if _, err := st.SavePushSubscription(ctx, s, 10); err == nil {
		t.Fatal("subscription saved for a login that does not exist")
	}
}

// A user gets at most rateMax alerts a minute (each tag once per minGap);
// a note that replaces an earlier one is never held back.
func TestSendLimitsAlertsPerUser(t *testing.T) {
	n, _ := newTestNotifier(t, Options{})
	ctx := context.Background()
	sent := 0
	for i := 0; i < 15; i++ {
		if n.Send(ctx, 1, Note{Title: "t", Tag: "tag-" + string(rune('a'+i))}) {
			sent++
		}
	}
	if sent != n.rateMax {
		t.Fatalf("%d of 15 sent, want %d", sent, n.rateMax)
	}
	if !n.Send(ctx, 2, Note{Title: "t", Tag: "x"}) {
		t.Fatal("another user's alert was held back")
	}
	if !n.Send(ctx, 1, Note{Title: "t", Tag: "tag-a", Replace: true}) || !n.Send(ctx, 1, Note{Title: "t", Tag: "tag-a", Replace: true}) {
		t.Fatal("a replacement was held back")
	}
	if n.Send(ctx, 2, Note{Title: "t", Tag: "x"}) {
		t.Fatal("the same tag within minGap was sent twice")
	}
	if !n.Send(ctx, 2, Note{Title: "t", Tag: "x", Distinct: true}) {
		t.Fatal("a distinct alert was collapsed")
	}
}

// A user's notes leave in the order they were sent, one at a time (a push
// service keeping the last note of a topic then keeps the newest), and carry
// increasing numbers.
func TestNotesLeaveInOrder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Title string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		time.Sleep(time.Duration(mrand.Intn(15)) * time.Millisecond) // (answers at their own pace)
		mu.Lock()
		got = append(got, b.Title)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	n, st := newTestNotifier(t, Options{WebhookPrefixes: []string{srv.URL + "/"}})
	n.http.Transport = srv.Client().Transport
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice", "", "x", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveUserSettings(ctx, u.ID, `{"webhook":"`+srv.URL+`/hook"}`); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := 0; i < 8; i++ {
		title := fmt.Sprintf("n%d", i)
		want = append(want, title)
		if !n.Send(ctx, u.ID, Note{Title: title, Tag: title}) {
			t.Fatal("held back")
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		done := len(got) == len(want)
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}
