package web

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/ihainan/corral/internal/convo"
)

// Subagents a Claude Code agent sent off in the background and that are still
// at it, by name, for the side list to hang under the agent (UI item 8, 11-25):
// the count alone is what its log's turn record says (bgProber); which they
// are, and what each was sent for, is what Claude Code keeps beside each
// subagent's own log — subagents/agent-<id>.meta.json: its description, how
// it was sent ("requestShape":"background"), and, once it is over,
// "finished":true (written since 2.1.302; before, not at all: none of those is
// told, rather than every one it ever sent). Read in the same look as the
// commands (cmdScanCommand), of the sessions watched only: those whose log was
// written to in the last six hours, so one of a Claude Code gone (never marked
// finished) drops out.

// bgAgent is a subagent at work in the background: its id (what its log is
// named by, for its conversation to be read), what it was sent for, and when
// (unix ms: when its meta was written, as it was sent).
// When Agent is non-empty ("agy", "claude", "codex"), this entry represents a
// recognized agent CLI running inside a background command (Task is the parent
// background command's task ID, Cmd its shell command line, and log its
// server-side verified session log path).
type bgAgent struct {
	ID    string `json:"id"`
	What  string `json:"what,omitempty"`
	Since int64  `json:"since,omitempty"`
	Agent string `json:"agent,omitempty"`
	Task  string `json:"task,omitempty"`
	Cmd   string `json:"cmd,omitempty"`
	log   string
}

const maxSubsShown = 8 // listed per agent

// subMinVersion: the first Claude Code that marks a subagent finished.
var subMinVersion = [3]int{2, 1, 302}

// parseBgSubScan reads the "sub" lines of what cmdScanCommand printed (those
// of the sessions it was given: at most subsScanned each): each agent
// session's subagents at work in the background, the latest sent first.
func parseBgSubScan(out string) map[string][]bgAgent {
	got := map[string][]bgAgent{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 6)
		if len(f) != 6 || f[0] != "sub" || !convoUUID.MatchString(f[1]) || !convo.SubagentID(f[2]) || !versionAtLeast(f[4], subMinVersion) {
			continue
		}
		// (Sent in the background and not finished, as the script told by its meta; of it, what it
		// was sent for only — cut short, not read: then said without.)
		var m struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal([]byte(f[5]), &m)
		a := bgAgent{ID: f[2], What: cmdTitle(m.Description)}
		if t, err := strconv.ParseInt(f[3], 10, 64); err == nil && t > 0 && t < 1<<40 {
			a.Since = t * 1000
		}
		session := strings.ToLower(f[1])
		if !slices.ContainsFunc(got[session], func(o bgAgent) bool { return o.ID == a.ID }) {
			got[session] = append(got[session], a)
		}
	}
	for _, l := range got {
		slices.SortStableFunc(l, func(a, b bgAgent) int { return int(min(max(b.Since-a.Since, -1), 1)) })
	}
	return got
}

// versionAtLeast: v ("2.1.331") is want or later; false where it is not one.
func versionAtLeast(v string, want [3]int) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return false
		}
		if n != want[i] {
			return n > want[i]
		}
	}
	return true
}

func sameSubs(a, b map[string][]bgAgent) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// subsShown: of the subagents found for a pane's agent, those to list, as what
// its turn record said (bg, n: see bgProber) allows — none where it said none
// are out; the latest n where it said n. While it works, or not known yet, all.
func subsShown(found []bgAgent, status, bg string, n int) []bgAgent {
	if len(found) == 0 {
		return nil
	}
	if status != "working" {
		switch bg {
		case bgNone:
			return nil
		case bgRunning:
			if n > 0 && n < len(found) {
				found = found[:n]
			}
		}
	}
	return found[:min(len(found), maxSubsShown)]
}
