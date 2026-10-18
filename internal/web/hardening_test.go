package web

// Regression tests for findings from the forwarding and notification review.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/ihainan/corral/internal/notify"
)

// withPush turns notifications on for the harness server.
func (h *harness) withPush() {
	h.t.Helper()
	n, err := notify.New(context.Background(), h.store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		notify.Options{LoginValid: h.auth.LoginValid})
	if err != nil {
		h.t.Fatal(err)
	}
	h.srv.notifier = n
}

func (h *harness) userID(c *client) int64 {
	h.t.Helper()
	return int64(c.do("GET", "/api/me", nil).json()["id"].(float64))
}

func (c *client) subscribe(endpoint string) resp {
	c.h.t.Helper()
	return c.do("POST", "/api/push/subscriptions", map[string]any{"endpoint": endpoint,
		"keys": map[string]string{"p256dh": "BPk", "auth": "a"}})
}

func (h *harness) endpoints(userID int64) map[string]bool {
	h.t.Helper()
	subs, err := h.store.PushSubscriptions(context.Background(), userID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, s := range subs {
		out[s.Endpoint] = true
	}
	return out
}

func (h *harness) login(name string) *client {
	h.t.Helper()
	c := &client{h: h}
	mustStatus(h.t, c.do("POST", "/api/login", map[string]string{"username": name, "password": name + "-password-1"}), 200, "login "+name)
	return c
}

const fcm = "https://fcm.googleapis.com/fcm/send/"

func TestPushSubscriptionsEndWithTheirLogin(t *testing.T) {
	h := newHarness(t)
	h.withPush()
	admin := h.admin()
	alice := h.member(admin, "alice")
	aliceID := h.userID(alice)

	// Signing out removes that browser's subscription only.
	phone := h.login("alice")
	mustStatus(t, alice.subscribe(fcm+"laptop"), 200, "subscribe laptop")
	mustStatus(t, phone.subscribe(fcm+"phone"), 200, "subscribe phone")
	mustStatus(t, alice.do("POST", "/api/logout", nil), 200, "logout")
	if got := h.endpoints(aliceID); got[fcm+"laptop"] || !got[fcm+"phone"] {
		t.Fatalf("after signing out the laptop: %v", got)
	}

	// A password change signs other browsers out, and their subscriptions with them.
	tablet := h.login("alice")
	mustStatus(t, tablet.subscribe(fcm+"tablet"), 200, "subscribe tablet")
	mustStatus(t, phone.do("POST", "/api/me/password", map[string]string{"current": "alice-password-1", "next": "alice-password-2"}), 200, "change password")
	if got := h.endpoints(aliceID); got[fcm+"tablet"] || !got[fcm+"phone"] {
		t.Fatalf("after a password change: %v", got)
	}

	// Registering an endpoint again moves it to the new user and login.
	bob := h.member(admin, "bob")
	bobID := h.userID(bob)
	mustStatus(t, bob.subscribe(fcm+"phone"), 200, "bob takes over the device")
	if h.endpoints(aliceID)[fcm+"phone"] || !h.endpoints(bobID)[fcm+"phone"] {
		t.Fatal("endpoint was not rebound to the new user")
	}
	mustStatus(t, bob.do("POST", "/api/logout", nil), 200, "bob logout")
	if len(h.endpoints(bobID)) != 0 {
		t.Fatal("rebound subscription survived the new login's sign-out")
	}

	// Disabling a user and an administrator's password reset end them too.
	mustStatus(t, phone.subscribe(fcm+"phone2"), 200, "subscribe again")
	mustStatus(t, admin.do("POST", fmt.Sprintf("/api/admin/users/%d/password", aliceID), map[string]string{"password": "reset-password-1"}), 200, "reset")
	if len(h.endpoints(aliceID)) != 0 {
		t.Fatal("subscription survived an administrator's password reset")
	}
	carol := h.member(admin, "carol")
	carolID := h.userID(carol)
	mustStatus(t, carol.subscribe(fcm+"carol"), 200, "subscribe carol")
	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", carolID), map[string]any{"disabled": true}), 200, "disable")
	if len(h.endpoints(carolID)) != 0 {
		t.Fatal("subscription survived disabling the user")
	}
}

func TestPushSubscriptionsPerUserAreCapped(t *testing.T) {
	old := maxPushSubscriptionsPerUser
	maxPushSubscriptionsPerUser = 3
	defer func() { maxPushSubscriptionsPerUser = old }()
	h := newHarness(t)
	h.withPush()
	admin := h.admin()
	for i := 0; i < 6; i++ {
		mustStatus(t, admin.subscribe(fmt.Sprintf("%sdev%d", fcm, i)), 200, "subscribe")
	}
	got := h.endpoints(h.userID(admin))
	if len(got) != 3 || !got[fcm+"dev5"] || got[fcm+"dev0"] {
		t.Fatalf("kept %v, want the 3 newest", got)
	}
	r := admin.do("GET", "/api/push", nil)
	if devs, _ := r.json()["devices"].([]any); len(devs) != 3 {
		t.Fatalf("device list: %s", r.Body)
	}
	if r.Status != http.StatusOK {
		t.Fatal(r.Status)
	}
}
