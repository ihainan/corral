package web

import (
	"encoding/json"
	"testing"

	"github.com/ihainan/corral/internal/herdr"
)

// herdr's snapshot names the agent session a pane runs; the gateway keeps it
// while the session watches, and forgets it when the watch ends.
func TestHerdrSids(t *testing.T) {
	var snap herdr.Snapshot
	raw := `{"panes":[{"pane_id":"w1:p1","agent":"claude","agent_session":{"agent":"claude","kind":"id","value":"8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1"}},{"pane_id":"w1:p2"}]}`
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatal(err)
	}
	h := herdrSockets{path: map[string]string{}}
	h.setSids("s1", &snap)
	if got := h.sid("s1", "w1:p1"); got != "" {
		t.Errorf("kept with no watch: %q", got)
	}
	h.set("s1", "/sock")
	h.setSids("s1", &snap)
	if got := h.sid("s1", "w1:p1"); got != "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1" {
		t.Errorf("w1:p1: %q", got)
	}
	if got := h.sid("s1", "w1:p2"); got != "" {
		t.Errorf("w1:p2: %q", got)
	}
	h.set("s1", "")
	if got := h.sid("s1", "w1:p1"); got != "" {
		t.Errorf("after the watch: %q", got)
	}
}
