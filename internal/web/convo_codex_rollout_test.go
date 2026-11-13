package web

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/convo"
)

// Codex 0.167.2 keeps the session ID in session_meta after rewind, and
// appends a separate writer segment ID to the new rollout's filename.
func TestCodexRewindRolloutIdentity(t *testing.T) {
	const session = "01a1cdc3-eed2-7582-b2a4-5acb0d4ec0e3"
	const segment = "01a1cdc3-f478-7880-8763-db08ea88aadb"
	base := "/home/u/.codex/sessions/2026/11/12/rollout-2026-11-12T11-18-53-" + session
	for _, name := range []string{base + ".jsonl", base + "_" + segment + ".jsonl"} {
		if got := convoSessionOf(name); got != session || !convoLogOf("codex", name, session) {
			t.Errorf("session identity lost for %s: %q", name, got)
		}
		if convoLogOf("codex", name, segment) {
			t.Errorf("writer segment accepted as the resumable session: %s", name)
		}
		out := parseConvoLocate(convoRec("pid=7", "start=70", "procstart=70", "sid="+session, "log="+name, "agent=codex")).procs
		if len(out) != 1 || out[0].log != name || out[0].sid != session {
			t.Errorf("located rollout discarded: %+v", out)
		}
	}
	for _, bad := range []string{base + "_not-a-segment.jsonl", base + "_" + segment + "_" + segment + ".jsonl", base + "x.jsonl"} {
		if convoSessionOf(bad) != "" || convoLogOf("codex", bad, session) {
			t.Errorf("malformed rollout accepted: %s", bad)
		}
	}
}

func TestCodexRewindHistoryNotice(t *testing.T) {
	const session = "01a1cdc3-eed2-7582-b2a4-5acb0d4ec0e3"
	const segment = "01a1cdc3-f478-7880-8763-db08ea88aadb"
	st := &convoState{agent: "codex", parser: convo.NewCodex()}
	base := "/home/u/.codex/sessions/2026/11/12/rollout-2026-11-12T11-18-53-" + session
	for _, tc := range []struct {
		path    string
		partial bool
	}{{base + ".jsonl", false}, {base + "_" + segment + ".jsonl", true}, {base + "_unknown.jsonl", false}} {
		st.log = tc.path
		if m := st.meta(); m.HistoryPartial != tc.partial || m.Forked {
			t.Errorf("history for %s: %+v", tc.path, m)
		}
	}
}

// The host's gate must use the same session identity as the gateway, so a
// message after rewind is accepted for its session and refused for its segment.
func TestCodexHostRolloutIdentity(t *testing.T) {
	const session = "01a1cdc3-eed2-7582-b2a4-5acb0d4ec0e3"
	const segment = "01a1cdc3-f478-7880-8763-db08ea88aadb"
	base := "/home/u/.codex/sessions/rollout-2026-11-12T11-18-53-" + session
	for _, name := range []string{base + ".jsonl", base + "_" + segment + ".jsonl", base + "_invalid.jsonl", base + "_" + segment + "_" + segment + ".jsonl"} {
		cmd := exec.Command("sh", "-c", codexFdsSh+"\ncodex_log_sid \"$1\"; printf '%s' \"$cxid\"", "sh", name)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != convoSessionOf(name) {
			t.Fatalf("host and gateway disagree on %s: %q, %v", name, out, err)
		}
	}
}
