package web

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/store"
)

func withRecording(t *testing.T) harnessOpt {
	dir := t.TempDir()
	return func(c *config.Config) {
		c.DataDir = dir
		c.Recording = true
		c.RecordingDays = 30
		c.RecordingMaxBytes = 1 << 20
	}
}

func waitFinished(t *testing.T, h *harness, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r, err := h.store.RecordingByID(t.Context(), id); err == nil && r.EndedAt != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("recording never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSessionsAreRecorded(t *testing.T) {
	h := newHarness(t, withRecording(t))
	admin := h.admin()
	bob := h.member(admin, "bob")
	carol := h.member(admin, "carol")
	srv := h.sshServer(t.TempDir())
	r := bob.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(bob, srv.Addr.Port)})
	mustStatus(t, r, 201, "open session")
	id := r.json()["id"].(string)
	if r.json()["recording"] != true {
		t.Fatalf("session does not say it is recorded: %s", r.Body)
	}
	sess, err := h.terms.Get(h.userID(bob), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Write(t.Context(), []byte("echo-me-中文\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// Still recording: listed, not deletable.
	if r := admin.do("DELETE", "/api/recordings/"+id, nil); r.Status != 409 {
		t.Fatalf("delete while recording: %d", r.Status)
	}
	mustStatus(t, bob.do("DELETE", "/api/sessions/"+id, nil), 200, "close session")
	waitFinished(t, h, id)

	list := bob.do("GET", "/api/recordings", nil).json()["recordings"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["owner_name"] != "bob" {
		t.Fatalf("bob's list: %v", list)
	}
	if n := len(carol.do("GET", "/api/recordings", nil).json()["recordings"].([]any)); n != 0 {
		t.Fatalf("carol sees %d recordings", n)
	}
	if n := len(admin.do("GET", "/api/recordings", nil).json()["recordings"].([]any)); n != 1 {
		t.Fatalf("admin sees %d recordings", n)
	}
	cast := bob.do("GET", "/api/recordings/"+id+"/cast", nil)
	mustStatus(t, cast, 200, "play own")
	if !strings.HasPrefix(string(cast.Body), `{"version":2,`) || !strings.Contains(string(cast.Body), "echo-me-中文") {
		t.Fatalf("cast: %.300s", cast.Body)
	}
	mustStatus(t, carol.do("GET", "/api/recordings/"+id+"/cast", nil), 404, "someone else's")
	mustStatus(t, admin.do("GET", "/api/recordings/"+id+"/cast", nil), 200, "admin plays")
	found := false
	for _, a := range auditActions(t, h) {
		found = found || a == "recording.view"
	}
	if !found {
		t.Fatal("watching is not audited")
	}
	mustStatus(t, bob.do("DELETE", "/api/recordings/"+id, nil), 403, "owner deletes")
	mustStatus(t, admin.do("DELETE", "/api/recordings/"+id, nil), 200, "admin deletes")
	if _, err := os.Stat(h.srv.recordingPath(id)); !os.IsNotExist(err) {
		t.Fatal("file left after delete")
	}
}

func TestOldRecordingsArePruned(t *testing.T) {
	h := newHarness(t, withRecording(t))
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	id := r.json()["id"].(string)
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+id, nil), 200, "close")
	waitFinished(t, h, id)
	h.srv.pruneRecordingsOnce()
	if _, err := h.store.RecordingByID(t.Context(), id); err != nil {
		t.Fatal("a fresh recording was pruned")
	}
	old := time.Now().AddDate(0, 0, -31).Unix()
	if err := h.store.SetRecordingStart(t.Context(), id, old); err != nil {
		t.Fatal(err)
	}
	h.srv.pruneRecordingsOnce()
	if _, err := h.store.RecordingByID(t.Context(), id); err == nil {
		t.Fatal("an old recording was kept")
	}
	if _, err := os.Stat(h.srv.recordingPath(id)); !os.IsNotExist(err) {
		t.Fatal("old file kept")
	}
}

func TestShutdownCompletesRecordings(t *testing.T) {
	h := newHarness(t, withRecording(t))
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	r := admin.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(admin, srv.Addr.Port)})
	id := r.json()["id"].(string)
	sess, _ := h.terms.Get(h.userID(admin), id)
	sess.Write(t.Context(), []byte("before-shutdown\n"))
	time.Sleep(200 * time.Millisecond)
	// As the gateway stops: terminals first, then the server.
	h.terms.Shutdown()
	h.srv.Close()
	rec, err := h.store.RecordingByID(t.Context(), id)
	if err != nil || rec.EndedAt == 0 || rec.Bytes <= 0 {
		t.Fatalf("recording after shutdown: %+v %v", rec, err)
	}
}

func TestStaleRecordingsAreFinished(t *testing.T) {
	h := newHarness(t, withRecording(t))
	ctx := t.Context()
	if err := h.store.CreateRecording(ctx, &store.Recording{ID: "deadbeefdeadbeef", OwnerID: 1, OwnerName: "x", HostID: 1, HostName: "h", StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	h.srv.finishStale()
	rec, _ := h.store.RecordingByID(ctx, "deadbeefdeadbeef")
	if rec.EndedAt == 0 || !rec.Truncated || rec.Bytes != -1 {
		t.Fatalf("stale recording: %+v", rec)
	}
}

// The list narrowed (pages-1105 3.1): by host and time for anyone, by owner
// for an admin only — a member asking for another's sees their own still;
// the choices are of what one may see, owners for an admin only.
func TestRecordingListFilters(t *testing.T) {
	h := newHarness(t, withRecording(t))
	admin := h.admin()
	bob := h.member(admin, "bob")
	carol := h.member(admin, "carol")
	bobID, carolID := h.userID(bob), h.userID(carol)
	for _, r := range []store.Recording{
		{ID: "r1", OwnerID: bobID, OwnerName: "bob", HostID: 7, HostName: "web", StartedAt: 1000, EndedAt: 1010},
		{ID: "r2", OwnerID: bobID, OwnerName: "bob", HostID: 8, HostName: "db", StartedAt: 2000, EndedAt: 2010},
		{ID: "r3", OwnerID: carolID, OwnerName: "carol", HostID: 7, HostName: "web", StartedAt: 3000, EndedAt: 3010},
	} {
		if err := h.store.CreateRecording(t.Context(), &r); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(c *client, q string) string {
		t.Helper()
		out := ""
		for _, x := range c.do("GET", "/api/recordings"+q, nil).json()["recordings"].([]any) {
			out += x.(map[string]any)["id"].(string)
		}
		return out
	}
	for _, c := range []struct {
		who  *client
		q    string
		want string
	}{
		{admin, "", "r3r2r1"},
		{admin, "?host_id=7", "r3r1"},
		{admin, "?owner_id=" + itoa(bobID), "r2r1"},
		{admin, "?from=1500&to=3000", "r2"},
		{bob, "?host_id=7", "r1"},
		{carol, "?owner_id=" + itoa(bobID), "r3"}, // (not bob's: hers)
	} {
		if got := ids(c.who, c.q); got != c.want {
			t.Errorf("%s: got %q, want %q", c.q, got, c.want)
		}
	}
	j := carol.do("GET", "/api/recordings?choices=1", nil).json()
	if hs := j["hosts"].([]any); len(hs) != 1 || j["owners"] != nil {
		t.Fatalf("carol's choices: %v", j)
	}
	j = admin.do("GET", "/api/recordings?choices=1", nil).json()
	if len(j["hosts"].([]any)) != 2 || len(j["owners"].([]any)) != 2 {
		t.Fatalf("admin's choices: %v", j)
	}
	// The audit list's kinds: a known one, "other", an unknown one refused.
	mustStatus(t, admin.do("GET", "/api/admin/audit?kind=account", nil), 200, "kind")
	mustStatus(t, admin.do("GET", "/api/admin/audit?kind=other", nil), 200, "other")
	mustStatus(t, admin.do("GET", "/api/admin/audit?kind=nope", nil), 400, "unknown kind")
	var list []map[string]any
	if err := json.Unmarshal(admin.do("GET", "/api/admin/audit?kind=account", nil).Body, &list); err != nil || len(list) == 0 {
		t.Fatalf("account entries: %v %v", list, err)
	}
	for _, e := range list {
		if a := e["action"].(string); !strings.HasPrefix(a, "login") && !strings.HasPrefix(a, "user") && !strings.HasPrefix(a, "logout") && !strings.HasPrefix(a, "setup") && !strings.HasPrefix(a, "invite") {
			t.Errorf("account kind has %s", a)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A user's audit entries take in the sign-ins tried as them that failed —
// written with no user, the name tried as their target (review cd07bb9).
func TestAuditByUserHasFailedSignIns(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	stranger := &client{h: h}
	if r := stranger.do("POST", "/api/login", map[string]string{"username": "bob", "password": "not-his-password"}); r.Status == 200 {
		t.Fatal("a wrong password signed in")
	}
	var list []map[string]any
	if err := json.Unmarshal(admin.do("GET", "/api/admin/audit?user="+itoa(h.userID(bob)), nil).Body, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range list {
		if e["action"] == "login.failed" && e["target"] == "bob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bob's entries have no failed sign-in: %v", list)
	}
}
