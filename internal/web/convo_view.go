package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/ihainan/corral/internal/sshx"
)

// Who has Claude Code's input box (plans/send-view.md).
//
// A message is sent by pasting it into the pane and pressing Enter: it goes
// to whatever Claude Code's interface has in front. Claude Code's session
// file only says whether it waits for an answer; it says nothing of a
// subagent's conversation brought to the front (its footer's list of
// agents, ↓ and Enter), of the background sessions' view (← on an empty
// input), or of that list holding the keys. Typed there, a message goes to
// the subagent, or starts a session of its own, and never reaches the
// conversation the page shows: the page waited for it for ever.
//
// So before anything is typed (and again before Enter) the host reads the
// screen and types only where the input box is plainly the conversation's
// own: positively, not by ruling out states known so far. What it is not
// sure of, it does not type into.
//
// The rules, of the bottom input box and what is under it only (never the
// thread above, which may quote any screen):
//   - the box: a line beginning with ❯ under a rule (one with a title too:
//     a renamed session's name is drawn there, so a title says nothing), and
//     a whole rule further down;
//   - under it, the list of agents: "◯ main" (the main conversation not the
//     one shown) → subagent; a row of it with the list's ❯ cursor, or
//     "↑/↓ to select" → list (the keys go to the list);
//     "enter to return" → sessions (the background sessions' view);
//   - its line (before typing: then it holds no text of ours): a placeholder
//     "Message @…" → subagent; "describe a task for a new session" →
//     sessions;
//   - no box → none. Else → main.
//
// Two of it: claudeOwner here (the look after a message did not turn up),
// claudeOwnerAwk on the host (the gate). The test runs both over the same
// screens, real ones (testdata/view), and wants them to agree.

// claudeOwner says who has the input box of the screen (plain text, as
// read). after: the box holds a text of ours (just pasted, or left there by
// an Enter lost): what it holds is not read — a message of the person's may
// begin "Message @…" too.
// (Words are matched with spaces taken out, across the lines they wrap to
// in a narrow pane.)
func claudeOwner(screen string, after bool) string {
	// (As the awk reads it: a CR only at a line's end taken off; blank, a
	// line of spaces, tabs and no-break spaces only — review551-3.)
	lines := strings.Split(screen, "\n")
	for k, l := range lines {
		lines[k] = strings.TrimSuffix(l, "\r")
	}
	n := len(lines)
	for n > 0 && ownerSqueeze(lines[n-1]) == "" {
		n--
	}
	for i := n - 1; i >= 1; i-- {
		if !ownerPrompt.MatchString(lines[i]) || !ownerRule.MatchString(lines[i-1]) {
			continue
		}
		end := -1
		for j := i + 1; j < n; j++ {
			if ownerRule.MatchString(lines[j]) {
				end = j
				break
			}
		}
		if end < 0 {
			continue
		}
		foot := lines[end+1 : n]
		for _, l := range foot {
			if ownerOtherMain.MatchString(l) {
				return "subagent"
			}
		}
		// (The footer's own words: the agents' rows aside — a subagent's
		// description may say anything — review552; only rows of the list
		// itself, not any row with a bullet — review554.)
		agents := ownerAgents(foot)
		var own []string
		for k, l := range foot {
			if !agents[k] {
				own = append(own, l)
			}
		}
		said := ownerSqueeze(strings.Join(own, ""))
		for _, l := range foot {
			if ownerListCursor.MatchString(l) {
				return "list"
			}
		}
		if strings.Contains(said, "\u2191/\u2193toselect") {
			return "list"
		}
		if strings.Contains(said, "entertoreturn") {
			return "sessions"
		}
		// (Under it, a dialog's or a picker's lines — a choice, its cursor,
		// its keys — or more than status lines: it is not the box in front.)
		other := 0
		for k, l := range foot {
			if agents[k] {
				continue
			}
			other++
			if strings.Contains(l, "\u276f") || ownerChoice.MatchString(l) {
				return "none"
			}
		}
		if strings.Contains(said, "Esctocancel") || strings.Contains(said, "Entertoselect") || strings.Contains(said, "Entertoconfirm") {
			return "none"
		}
		if other > ownerFoot {
			return "none"
		}
		if !after {
			box := ownerSqueeze(ownerPrompt.ReplaceAllString(strings.Join(lines[i:end], ""), ""))
			if strings.HasPrefix(box, "Message@") {
				return "subagent"
			}
			if box == "describeataskforanewsession" {
				return "sessions"
			}
		}
		return "main"
	}
	return "none"
}

// ownerAgents marks the rows of the footer that are the list of agents: a
// run of rows each shaped as one ("● main", "◯ general-purpose  what it
// does"), one of them main. A row with a bullet elsewhere (a picker's, a
// hint drawn with one) is not.
func ownerAgents(foot []string) []bool {
	mask := make([]bool, len(foot))
	for k := 0; k < len(foot); {
		if !ownerAgentRow.MatchString(foot[k]) {
			k++
			continue
		}
		e, hasMain := k, false
		for e < len(foot) && ownerAgentRow.MatchString(foot[e]) {
			hasMain = hasMain || ownerMainRow.MatchString(foot[e])
			e++
		}
		for x := k; x < e && hasMain; x++ {
			mask[x] = true
		}
		k = e
	}
	return mask
}

// ownerFoot: how many status lines under the box (the list of agents aside)
// are taken for its own.
const ownerFoot = 12

func ownerSqueeze(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\u00a0' {
			return -1
		}
		return r
	}, s)
}

var (
	ownerRule       = regexp.MustCompile(`^[ \t]*(?:[─]{8}|[━]{8})`)
	ownerPrompt     = regexp.MustCompile(`^[ \t]*❯`)
	ownerOtherMain  = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?◯ main(?:[ \t]|$)`)
	ownerListCursor = regexp.MustCompile(`^[ \t]*❯[ \t]+[◯●] `)
	ownerAgentRow   = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?[◯●] [^ \t]+(?:[ \t][ \t]+.*|[ \t]*)$`)
	ownerMainRow    = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?[◯●] main[ \t]*$`)
	ownerChoice     = regexp.MustCompile(`^[ \t]*\d+\.[ \t]`)
)

// claudeOwnerAwk is claudeOwner as an awk program (POSIX: busybox's, mawk,
// gawk; any locale): the screen on its input, after=1 for "after", the
// owner printed. (Its characters as bytes: ❯ ◯ ● ─ ━ ↑ ↓, and the no-break
// space put in by the gateway, NBSP below.)
var claudeOwnerAwk = strings.ReplaceAll(`function isrule(s) { sub(/^[ \t]*/, "", s); return index(s, "────────") == 1 || index(s, "━━━━━━━━") == 1 }
function isprompt(s) { sub(/^[ \t]*/, "", s); return index(s, "❯") == 1 }
function squeeze(s) { gsub(/[ \t]/, "", s); gsub(/NBSP/, "", s); return s }
function agentrow(s) { return s ~ /^[ \t]*(❯[ \t]+)?(◯|●) [^ \t]+([ \t][ \t]+.*|[ \t]*)$/ }
function mainrow(s) { return s ~ /^[ \t]*(❯[ \t]+)?(◯|●) main[ \t]*$/ }
{ sub(/\r$/, ""); L[NR] = $0 }
END {
  n = NR
  while (n > 0 && squeeze(L[n]) == "") n--
  for (i = n; i >= 2; i--) {
    if (!isprompt(L[i]) || !isrule(L[i-1])) continue
    e = 0
    for (j = i + 1; j <= n; j++) if (isrule(L[j])) { e = j; break }
    if (!e) continue
    for (j = e + 1; j <= n; j++) if (L[j] ~ /^[ \t]*(❯[ \t]+)?◯ main([ \t]|$)/) { print "subagent"; exit }
    for (j = e + 1; j <= n; j++) A[j] = 0
    j = e + 1
    while (j <= n) {
      if (!agentrow(L[j])) { j++; continue }
      k = j; m = 0
      while (k <= n && agentrow(L[k])) { if (mainrow(L[k])) m = 1; k++ }
      if (m) for (x = j; x < k; x++) A[x] = 1
      j = k
    }
    f = ""
    for (j = e + 1; j <= n; j++) {
      if (L[j] ~ /^[ \t]*❯[ \t]+(◯|●) /) { print "list"; exit }
      if (!A[j]) f = f squeeze(L[j])
    }
    if (index(f, "↑/↓toselect")) { print "list"; exit }
    if (index(f, "entertoreturn")) { print "sessions"; exit }
    other = 0
    for (j = e + 1; j <= n; j++) {
      if (A[j]) continue
      other++
      if (index(L[j], "❯") || L[j] ~ /^[ \t]*[0-9]+\.[ \t]/) { print "none"; exit }
    }
    if (index(f, "Esctocancel") || index(f, "Entertoselect") || index(f, "Entertoconfirm")) { print "none"; exit }
    if (other > FOOT) { print "none"; exit }
    if (after != 1) {
      b = ""
      for (j = i; j < e; j++) { t = L[j]; if (j == i) sub(/^[ \t]*❯/, "", t); b = b squeeze(t) }
      if (index(b, "Message@") == 1) { print "subagent"; exit }
      if (b == "describeataskforanewsession") { print "sessions"; exit }
    }
    print "main"; exit
  }
  print "none"
}`, "NBSP", "\u00a0")

func init() { claudeOwnerAwk = strings.ReplaceAll(claudeOwnerAwk, "FOOT", strconv.Itoa(ownerFoot)) }

// The gate's exits for who has the box (convoGated: 13–15 before anything
// is typed, 16 typed and Enter held back).
const (
	exitOwnerSubagent = 13
	exitOwnerOther    = 14
	exitOwnerNone     = 15
	exitOwnerTyped    = 16
)

// ownerRefusal: the error of a gate's exit for who has the input box
// (code, message, and whether the text was typed), ok false for any other.
func ownerRefusal(err error) (code, msg string, typed, ok bool) {
	var exitErr *sshx.ExitError
	if !errors.As(err, &exitErr) {
		return "", "", false, false
	}
	switch exitErr.Status {
	case exitOwnerSubagent:
		return "subagent_view", "the terminal shows a subagent's conversation: a message would go to it", false, true
	case exitOwnerOther:
		return "other_view", "the terminal's keys go elsewhere (the list of agents or of sessions)", false, true
	case exitOwnerNone:
		return "no_input", "the conversation's input box is not on the screen", false, true
	case exitOwnerTyped:
		return "typed_other_view", "typed into its input, not sent: the terminal went to another view before Enter", true, true
	}
	return "", "", false, false
}

// writeOwnerRefusal writes it (409), true where it was one.
func writeOwnerRefusal(w http.ResponseWriter, err error) (code string, typed, ok bool) {
	code, msg, typed, ok := ownerRefusal(err)
	if ok {
		writeError(w, http.StatusConflict, code, msg)
	}
	return code, typed, ok
}
