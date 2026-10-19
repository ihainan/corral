package web

import (
	"strings"
	"testing"
)

// Previews: the owner's windows send the screen as text; the list says when
// the latest was sent, and only the owner reads or writes it.
func TestSessionPreview(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	bob := h.member(admin, "bob")
	carol := h.member(admin, "carol")
	srv := h.sshServer(t.TempDir())
	r := bob.do("POST", "/api/sessions", map[string]any{"host_id": h.hostFor(bob, srv.Addr.Port)})
	mustStatus(t, r, 201, "open")
	id := r.json()["id"].(string)
	path := "/api/sessions/" + id + "/preview"

	mustStatus(t, bob.do("GET", path, nil), 404, "none yet")
	if l := bob.do("GET", "/api/sessions", nil).jsonArray(); l[0]["preview_rev"] != nil {
		t.Fatalf("preview_rev before any: %v", l[0]["preview_rev"])
	}
	mustStatus(t, bob.do("PUT", path, map[string]any{"cols": 80, "lines": []string{"$ ls\x1b[31m", "a\tb", "\xff"}}), 204, "put")
	p := bob.do("GET", path, nil)
	mustStatus(t, p, 200, "get")
	lines := p.json()["lines"].([]any)
	if len(lines) != 3 || lines[0] != "$ ls [31m" || lines[1] != "a b" || lines[2] != " " || p.json()["cols"] != float64(80) {
		t.Fatalf("stored preview: %s", p.Body)
	}
	if l := bob.do("GET", "/api/sessions", nil).jsonArray(); l[0]["preview_rev"] != float64(1) || p.json()["rev"] != float64(1) {
		t.Fatalf("revision after one preview: list %v, preview %v", l[0]["preview_rev"], p.json()["rev"])
	}
	// Two within a second are told apart.
	mustStatus(t, bob.do("PUT", path, map[string]any{"cols": 80, "lines": []string{"next"}}), 204, "put again")
	if l := bob.do("GET", "/api/sessions", nil).jsonArray(); l[0]["preview_rev"] != float64(2) {
		t.Fatalf("revision after two: %v", l[0]["preview_rev"])
	}

	// From one window, an older preview arriving late is dropped; another
	// window's is kept.
	put := func(c *client, win string, seq int, line string) {
		mustStatus(t, c.do("PUT", path, map[string]any{"cols": 80, "lines": []string{line}, "win": win, "seq": seq}), 204, "put "+line)
	}
	first := func() any { return bob.do("GET", path, nil).json()["lines"].([]any)[0] }
	put(bob, "w1", 5, "newer")
	put(bob, "w1", 4, "older")
	if got := first(); got != "newer" {
		t.Fatalf("a late older preview replaced the newer one: %v", got)
	}
	put(bob, "w2", 1, "other window")
	if got := first(); got != "other window" {
		t.Fatalf("another window's preview: %v", got)
	}
	// Still dropped after another window's came in between.
	put(bob, "w1", 3, "older still")
	if got := first(); got != "other window" {
		t.Fatalf("a late preview of the first window, after another's: %v", got)
	}

	// Others cannot read or replace it.
	mustStatus(t, carol.do("GET", path, nil), 404, "other user reads")
	mustStatus(t, carol.do("PUT", path, map[string]any{"cols": 80, "lines": []string{"x"}}), 404, "other user writes")

	// Sizes are bounded.
	many := make([]string, previewMaxLines+1)
	mustStatus(t, bob.do("PUT", path, map[string]any{"cols": 80, "lines": many}), 400, "too many lines")
	mustStatus(t, bob.do("PUT", path, map[string]any{"cols": 80, "lines": []string{strings.Repeat("x", 2*previewMaxCols+1)}}), 400, "line too long")
	mustStatus(t, bob.do("PUT", path, map[string]any{"cols": 0, "lines": []string{"x"}}), 400, "no width")

	// Not after the session ended.
	mustStatus(t, bob.do("DELETE", "/api/sessions/"+id, nil), 200, "close")
	if r := bob.do("PUT", path, map[string]any{"cols": 80, "lines": []string{"x"}}); r.Status != 404 && r.Status != 409 {
		t.Fatalf("put after close: %d", r.Status)
	}
}
