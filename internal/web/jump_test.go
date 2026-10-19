package web

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

// jumpEnv: a jump host J (password saved), and the address of a target that
// J reaches as "localhost".
type jumpEnv struct {
	h      *harness
	admin  *client
	jsrv   *testutil.SSHServer
	target *testutil.SSHServer
	jID    float64
}

func newJumpEnv(t *testing.T) *jumpEnv {
	h := newHarness(t)
	e := &jumpEnv{h: h, admin: h.admin(), jsrv: h.sshServer(t.TempDir()), target: h.sshServer(t.TempDir())}
	r := e.admin.do("POST", "/api/hosts", map[string]any{"name": "jump", "address": "127.0.0.1", "port": e.jsrv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none"})
	mustStatus(t, r, 201, "jump host")
	e.jID = r.json()["id"].(float64)
	return e
}

func (e *jumpEnv) host(name string, jump any) resp {
	return e.admin.do("POST", "/api/hosts", map[string]any{"name": name, "address": "localhost", "port": e.target.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none", "jump_host_id": jump})
}

func (e *jumpEnv) open(id float64) resp {
	return e.admin.do("POST", "/api/sessions", map[string]any{"host_id": int64(id)})
}

func TestSessionThroughJumpHost(t *testing.T) {
	e := newJumpEnv(t)
	r := e.host("inner", e.jID)
	mustStatus(t, r, 201, "host through the jump host")
	inner := r.json()["id"].(float64)
	if r.json()["jump_host_id"] != e.jID {
		t.Fatalf("jump_host_id not kept: %s", r.Body)
	}

	// The jump host's key is not known yet: connect to it directly first.
	r = e.open(inner)
	if r.Status == 201 || r.code() != "jump_not_trusted" {
		t.Fatalf("untrusted jump host: %d %s", r.Status, r.Body)
	}
	r = e.open(e.jID)
	mustStatus(t, r, 201, "jump host directly")
	e.admin.do("DELETE", "/api/sessions/"+r.json()["id"].(string), nil)

	r = e.open(inner)
	mustStatus(t, r, 201, "through the jump host")
	e.admin.do("DELETE", "/api/sessions/"+r.json()["id"].(string), nil)

	// The jump host's key changes: reported as the jump host's problem, not
	// as the target's (which must not be offered for acceptance).
	e.jsrv.RotateHostKey()
	r = e.open(inner)
	if r.Status == 201 || r.code() != "jump_key_changed" {
		t.Fatalf("jump host key changed: %d %s", r.Status, r.Body)
	}

	// A host others are reached through cannot be deleted.
	r = e.admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", int64(e.jID)), nil)
	mustStatus(t, r, 409, "delete jump host in use")
	if r.code() != "jump_in_use" {
		t.Fatalf("delete: %s", r.Body)
	}
	mustStatus(t, e.admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", int64(inner)), nil), 200, "delete the inner host")
	mustStatus(t, e.admin.do("DELETE", fmt.Sprintf("/api/hosts/%d", int64(e.jID)), nil), 200, "delete the jump host, now unused")
}

func TestJumpHostRules(t *testing.T) {
	e := newJumpEnv(t)
	r := e.host("inner", e.jID)
	mustStatus(t, r, 201, "host through the jump host")
	inner := r.json()["id"].(float64)
	field := func(r resp) string {
		x, _ := r.json()["extra"].(map[string]any)
		f, _ := x["field"].(string)
		return f
	}

	// Through a host that is itself reached through another: refused.
	r = e.host("deeper", inner)
	if r.Status != 422 || field(r) != "jump_host_id" {
		t.Fatalf("two levels: %d %s", r.Status, r.Body)
	}
	// The jump host cannot get a jump host while others go through it.
	r = e.admin.do("PUT", fmt.Sprintf("/api/hosts/%d", int64(e.jID)), map[string]any{"name": "jump", "address": "127.0.0.1", "port": e.jsrv.Addr.Port,
		"username": "alice", "auth": "password", "persistence": "none", "jump_host_id": inner})
	if r.Status != 422 || field(r) != "jump_host_id" {
		t.Fatalf("jump host with a jump host: %d %s", r.Status, r.Body)
	}
	// ...nor lose its saved credential.
	r = e.admin.do("PUT", fmt.Sprintf("/api/hosts/%d", int64(e.jID)), map[string]any{"name": "jump", "address": "127.0.0.1", "port": e.jsrv.Addr.Port,
		"username": "alice", "auth": "ask", "persistence": "none"})
	if r.Status != 422 || field(r) != "auth" {
		t.Fatalf("jump host to ask: %d %s", r.Status, r.Body)
	}
	// Itself.
	r = e.admin.do("PUT", fmt.Sprintf("/api/hosts/%d", int64(inner)), map[string]any{"name": "inner", "address": "localhost", "port": e.target.Addr.Port,
		"username": "alice", "auth": "password", "persistence": "none", "jump_host_id": inner})
	if r.Status != 422 || field(r) != "jump_host_id" {
		t.Fatalf("through itself: %d %s", r.Status, r.Body)
	}
	// A host that asks for its password cannot be a jump host.
	r = e.admin.do("POST", "/api/hosts", map[string]any{"name": "asker", "address": "127.0.0.1", "port": e.jsrv.Addr.Port,
		"username": "alice", "auth": "ask", "persistence": "none"})
	mustStatus(t, r, 201, "asking host")
	r = e.host("via-asker", r.json()["id"])
	if r.Status != 422 || field(r) != "jump_host_id" {
		t.Fatalf("through an asking host: %d %s", r.Status, r.Body)
	}
	// Someone else's host is no jump host.
	bob := e.h.member(e.admin, "bob")
	r = bob.do("POST", "/api/hosts", map[string]any{"name": "mine", "address": "localhost", "port": e.target.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none", "jump_host_id": e.jID})
	if r.Status != 422 || field(r) != "jump_host_id" {
		t.Fatalf("another user's jump host: %d %s", r.Status, r.Body)
	}
	// Taking the jump host away works.
	r = e.admin.do("PUT", fmt.Sprintf("/api/hosts/%d", int64(inner)), map[string]any{"name": "inner", "address": "127.0.0.1", "port": e.target.Addr.Port,
		"username": "alice", "auth": "password", "persistence": "none", "jump_host_id": nil})
	mustStatus(t, r, 200, "no jump host")
	if r.json()["jump_host_id"] != nil {
		t.Fatalf("jump host kept: %s", r.Body)
	}
}

// The rules hold in the statement that saves, whatever was checked before
// (two edits at once): no chains, and a jump host keeps a saved credential.
func TestJumpRulesHoldInTheStore(t *testing.T) {
	e := newJumpEnv(t)
	r := e.host("inner", e.jID)
	mustStatus(t, r, 201, "host through the jump host")
	ctx := t.Context()
	uid := e.h.userID(e.admin)
	inner, err := e.h.store.Host(ctx, uid, int64(r.json()["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	jump, err := e.h.store.Host(ctx, uid, int64(e.jID))
	if err != nil {
		t.Fatal(err)
	}
	// Through a host that is itself reached through another.
	deeper := *inner
	deeper.ID, deeper.Name, deeper.JumpHostID = 0, "deeper", &inner.ID
	if _, err := e.h.store.CreateHost(ctx, &deeper); !errors.Is(err, store.ErrJumpConflict) {
		t.Fatalf("chain created: %v", err)
	}
	// The jump host to 'ask', or getting a jump host itself, while used.
	j := *jump
	j.Auth, j.CredentialID = store.AuthAsk, nil
	if err := e.h.store.UpdateHost(ctx, &j, true); !errors.Is(err, store.ErrJumpConflict) {
		t.Fatalf("jump host to ask: %v", err)
	}
	j = *jump
	j.JumpHostID = &inner.ID
	if err := e.h.store.UpdateHost(ctx, &j, true); !errors.Is(err, store.ErrJumpConflict) {
		t.Fatalf("cycle: %v", err)
	}
	// Itself.
	self := *inner
	self.JumpHostID = &inner.ID
	if err := e.h.store.UpdateHost(ctx, &self, true); !errors.Is(err, store.ErrJumpConflict) {
		t.Fatalf("through itself: %v", err)
	}
	// An ordinary edit still saves.
	ok := *inner
	ok.Name = "renamed"
	if err := e.h.store.UpdateHost(ctx, &ok, false); err != nil {
		t.Fatal(err)
	}
}
