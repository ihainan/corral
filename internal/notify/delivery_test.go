package notify

// Delivery lifecycle: rebinding, webhooks, logins that cannot be checked.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/store"
)

// subscribe binds endpoint to a new login of a new user, returning the stored row.
func subscribe(t *testing.T, st *store.Store, user, login, endpoint string) *store.PushSubscription {
	t.Helper()
	ctx := context.Background()
	u, err := st.UserByName(ctx, user)
	if err != nil {
		if u, err = st.CreateUser(ctx, user, "", "x", store.RoleMember); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateLoginSession(ctx, login, u.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	s := testSubscription(t, endpoint)
	s.UserID, s.LoginHash = u.ID, login
	if _, err := st.SavePushSubscription(ctx, s, 10); err != nil {
		t.Fatal(err)
	}
	subs, _ := st.PushSubscriptions(ctx, u.ID)
	for _, got := range subs {
		if got.Endpoint == endpoint {
			return got
		}
	}
	t.Fatal("subscription not stored")
	return nil
}

// A device registered again by someone else: deliveries of the old binding
// stop (in flight or not yet started), and none of them touches the new one.
func TestReboundEndpointStopsTheOldOwnersDeliveries(t *testing.T) {
	n, st := newTestNotifier(t, Options{ExtraPushHosts: []string{"push.test"},
		LoginValid: func(context.Context, string) (bool, error) { return true, nil }})
	tr := stallTransport{entered: make(chan string, 4)}
	n.http.Transport = tr
	ctx := context.Background()
	old := subscribe(t, st, "alice", "alice-phone", "https://push.test/dev")

	done := make(chan struct{})
	go func() { n.pushOne(ctx, old.UserID, 0, old, []byte(`{}`), Note{}); close(done) }()
	<-tr.entered
	bob := subscribe(t, st, "bob", "bob-phone", "https://push.test/dev") // the device changes hands
	n.Rebound(bob.Endpoint, bob.Version)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery to the previous owner was not cancelled")
	}
	// A delivery that read the old binding before the change sends nothing.
	go func() { n.pushOne(ctx, old.UserID, 0, old, []byte(`{}`), Note{}) }()
	select {
	case <-tr.entered:
		t.Fatal("the previous owner's note was sent to the rebound device")
	case <-time.After(200 * time.Millisecond):
	}
	// Clean-up for the old binding leaves the new one alone.
	_ = st.MarkPushResult(ctx, old.ID, old.Version, false)
	_ = st.DeletePushSubscriptionHard(ctx, old.ID, old.Version)
	if subs, _ := st.PushSubscriptions(ctx, bob.UserID); len(subs) != 1 {
		t.Fatal("the old delivery's clean-up removed the new binding")
	}
}

// Versions never repeat, also after the rows that used them are gone: a
// delivery to a deleted registration is cancelled when the endpoint is
// registered again, even if nothing else is left in the table.
func TestReRegisteringAfterDeleteCancelsTheOldDelivery(t *testing.T) {
	n, st := newTestNotifier(t, Options{ExtraPushHosts: []string{"push.test"},
		LoginValid: func(context.Context, string) (bool, error) { return true, nil }})
	tr := stallTransport{entered: make(chan string, 4)}
	n.http.Transport = tr
	ctx := context.Background()
	old := subscribe(t, st, "alice", "alice-phone", "https://push.test/dev")
	done := make(chan struct{})
	go func() { n.pushOne(ctx, old.UserID, 0, old, []byte(`{}`), Note{}); close(done) }()
	<-tr.entered
	if err := st.DeletePushSubscription(ctx, old.UserID, old.Endpoint); err != nil { // the table is empty now
		t.Fatal(err)
	}
	bob := subscribe(t, st, "bob", "bob-phone", "https://push.test/dev")
	if bob.Version <= old.Version {
		t.Fatalf("version went from %d to %d", old.Version, bob.Version)
	}
	n.Rebound(bob.Endpoint, bob.Version)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery to the deleted registration was not cancelled")
	}
}

// Registration callbacks can run out of order: a late one for an older
// registration must not cancel deliveries to the newer one.
func TestLateReboundLeavesNewerDeliveriesAlone(t *testing.T) {
	n, st := newTestNotifier(t, Options{ExtraPushHosts: []string{"push.test"},
		LoginValid: func(context.Context, string) (bool, error) { return true, nil }})
	tr := stallTransport{entered: make(chan string, 4)}
	n.http.Transport = tr
	ctx := context.Background()
	v1 := subscribe(t, st, "alice", "alice-phone", "https://push.test/dev")
	v2 := subscribe(t, st, "bob", "bob-phone", "https://push.test/dev")
	n.Rebound(v2.Endpoint, v2.Version)
	done := make(chan struct{})
	go func() { n.pushOne(ctx, v2.UserID, 0, v2, []byte(`{}`), Note{}); close(done) }()
	<-tr.entered
	n.Rebound(v1.Endpoint, v1.Version) // the first registration's callback, late
	select {
	case <-done:
		t.Fatal("a late callback for an older registration cancelled the newer delivery")
	case <-time.After(200 * time.Millisecond):
	}
	n.Revoke(v2.UserID)
	<-done
}

// A webhook in flight is cancelled when the user loses access.
func TestRevokingAUserCancelsItsWebhookInFlight(t *testing.T) {
	n, st := newTestNotifier(t, Options{WebhookPrefixes: []string{"https://hooks.test/"}})
	tr := stallTransport{entered: make(chan string, 1)}
	n.http.Transport = tr
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "alice", "", "x", store.RoleMember)
	if err := st.SaveUserSettings(ctx, u.ID, `{"webhook":"https://hooks.test/alice"}`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { n.sendWebhook(ctx, u.ID, 0, Note{Title: "t"}); close(done) }()
	<-tr.entered
	n.Revoke(u.ID)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook in flight was not cancelled")
	}
	// A password change elsewhere is not a reason to drop the user's webhook.
	go func() { n.sendWebhook(ctx, u.ID, 1, Note{Title: "t"}) }()
	<-tr.entered
	n.RevokeOtherLogins(u.ID, "laptop")
	n.RevokeLogin(u.ID, "laptop")
	time.Sleep(100 * time.Millisecond)
	n.mu.Lock()
	inFlight := len(n.sending)
	n.mu.Unlock()
	if inFlight != 1 {
		t.Fatal("a login-level revocation cancelled the user's webhook")
	}
	n.Revoke(u.ID)
}

// Only a login known to be over removes a device; one that could not be
// checked (database trouble, cancelled check) keeps it.
func TestUncheckableLoginKeepsTheSubscription(t *testing.T) {
	var hits atomic.Int32
	n, st := newTestNotifier(t, Options{ExtraPushHosts: []string{"push.test"},
		LoginValid: func(context.Context, string) (bool, error) { return false, errors.New("database is busy") }})
	n.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		hits.Add(1)
		return nil, errors.New("unexpected push")
	})
	s := subscribe(t, st, "alice", "alice-phone", "https://push.test/dev")
	n.deliver(context.Background(), s.UserID, 0, Note{})
	if subs, _ := st.PushSubscriptions(context.Background(), s.UserID); len(subs) != 1 || hits.Load() != 0 {
		t.Fatalf("%d subscriptions left, %d pushes", len(subs), hits.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
