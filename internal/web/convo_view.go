package web

import (
	"bytes"
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
		// (The footer's words, all of them, the agents' rows too: telling a
		// row of the list from a picker's or a hint's drawn alike cannot be
		// done for sure (review552/554/556), and where unsure it refuses — a
		// subagent whose description happens to hold a hint's words ("Enter
		// to confirm") costs a refusal that says why, never a message sent
		// elsewhere. The list's rows are left out of the count of status
		// lines only: many subagents are no dialog.)
		agents := ownerAgents(foot)
		said := ownerSqueeze(strings.Join(foot, ""))
		// (A ❯ on a row of the list where the box's own status line is drawn
		// over it: where it was last clicked or tapped (a mouse-reporting
		// terminal), not the list's cursor — that one takes the status line's
		// place with its keys. Read as the cursor, it refused every message
		// once main's row had been tapped to go back to it — agent-view.md.)
		hover := ownerBoxStatus(foot, agents)
		for k, l := range foot {
			if ownerListCursor.MatchString(l) && !(hover && agents[k]) {
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
			if (strings.Contains(l, "\u276f") && !(hover && agents[k])) || ownerChoice.MatchString(l) {
				return "none"
			}
			if !agents[k] {
				other++
			}
		}
		if strings.Contains(said, "Esctocancel") || strings.Contains(said, "Entertoselect") || strings.Contains(said, "Entertoconfirm") {
			return "none"
		}
		if other > ownerFoot {
			return "none"
		}
		// (The list showing main as the one shown ("● main"): the box is the
		// conversation's, what it holds a draft of the person's whatever its
		// words — review561.)
		shown := false
		for k, l := range foot {
			if agents[k] && ownerMainRow.MatchString(l) && strings.Contains(l, "●") {
				shown = true
			}
		}
		if !after && !shown {
			if ownerPlaceholder(lines[i:end]) {
				return "subagent"
			}
			box := ownerSqueeze(ownerPrompt.ReplaceAllString(strings.Join(lines[i:end], ""), ""))
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

// ownerKeysLine: the line under the box saying what its keys do — the last
// one not blank above the list of agents (the box's own status line: its
// permission mode, "⏵⏵ auto mode on · …", "⏸ manual mode on · …", or "?
// for shortcuts" alone; the list's keys "↑/↓ to select · …" in its place
// while the list has them); "" where there is no list. Only that line, by
// how it begins: words of a subagent's description, or of a picker's
// title, say nothing (review559).
func ownerKeysLine(foot []string, agents []bool) string {
	last := ""
	for k, l := range foot {
		if agents[k] {
			return last
		}
		if ownerSqueeze(l) != "" {
			last = l
		}
	}
	return ""
}

// ownerBoxStatus: that line is the box's own: the box has the keys (a ❯ on
// a row of the list is where it was clicked, not the list's cursor).
func ownerBoxStatus(foot []string, agents []bool) bool {
	return ownerModeLine(ownerKeysLine(foot, agents))
}

func ownerModeLine(l string) bool {
	t := strings.TrimLeft(l, " \t\u00a0")
	return strings.HasPrefix(t, "⏵") || strings.HasPrefix(t, "⏸") || strings.HasPrefix(ownerSqueeze(l), "?forshortcuts")
}

// ownerPlaceholder: the box (its lines, the prompt's first) holds a
// subagent's placeholder and nothing else: "Message @<name>…" whole, its
// name of the letters agents' names have, its lines' blanks at their ends
// as one space (a narrow pane wraps it). A draft of the person beginning
// with those words is not one — review560/562.
func ownerPlaceholder(box []string) bool {
	parts := make([]string, 0, len(box))
	for k, l := range box {
		if k == 0 {
			l = ownerPrompt.ReplaceAllString(l, "")
		}
		if l = strings.Trim(l, " \t\u00a0"); l != "" {
			parts = append(parts, l)
		}
	}
	return ownerPlaceholderRe.MatchString(strings.Join(parts, " "))
}

// (A long name is cut and ends in "…" of its own before the placeholder's:
// "Message @code-modernization:……" — review563.)
var ownerPlaceholderRe = regexp.MustCompile(`^Message @[A-Za-z0-9_.:-]+…?…$`)

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
function placeholder(i, e,  j, t, r) { r = ""; for (j = i; j < e; j++) { t = L[j]; if (j == i) sub(/^[ \t]*❯/, "", t); sub(/^([ \t]|NBSP)+/, "", t); sub(/([ \t]|NBSP)+$/, "", t); if (t != "") r = (r == "" ? t : r " " t) }; return r ~ /^Message @[A-Za-z0-9_.:-]+(…)?…$/ }
function modeline(s,  t) { t = s; sub(/^[ \tNBSP]*/, "", t); return index(t, "⏵") == 1 || index(t, "⏸") == 1 || index(squeeze(s), "?forshortcuts") == 1 }
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
    hv = 0; kl = ""
    for (j = e + 1; j <= n; j++) {
      if (A[j]) { hv = modeline(kl); break }
      if (squeeze(L[j]) != "") kl = L[j]
    }
    f = ""
    for (j = e + 1; j <= n; j++) {
      if (L[j] ~ /^[ \t]*❯[ \t]+(◯|●) / && !(hv && A[j])) { print "list"; exit }
      f = f squeeze(L[j])
    }
    if (index(f, "↑/↓toselect")) { print "list"; exit }
    if (index(f, "entertoreturn")) { print "sessions"; exit }
    other = 0
    for (j = e + 1; j <= n; j++) {
      if ((index(L[j], "❯") && !(hv && A[j])) || L[j] ~ /^[ \t]*[0-9]+\.[ \t]/) { print "none"; exit }
      if (!A[j]) other++
    }
    if (index(f, "Esctocancel") || index(f, "Entertoselect") || index(f, "Entertoconfirm")) { print "none"; exit }
    if (other > FOOT) { print "none"; exit }
    sh = 0
    for (j = e + 1; j <= n; j++) if (A[j] && mainrow(L[j]) && L[j] ~ /●/) sh = 1
    if (after != 1 && !sh) {
      b = ""
      for (j = i; j < e; j++) { t = L[j]; if (j == i) sub(/^[ \t]*❯/, "", t); b = b squeeze(t) }
      if (placeholder(i, e)) { print "subagent"; exit }
      if (b == "describeataskforanewsession") { print "sessions"; exit }
    }
    print "main"; exit
  }
  print "none"
}`, "NBSP", "\u00a0")

func init() { claudeOwnerAwk = strings.ReplaceAll(claudeOwnerAwk, "FOOT", strconv.Itoa(ownerFoot)) }

// claudeFootAwk reads what the gate needs to bring the conversation's own
// box back in front (agent-view.md), off the bottom input box of Claude
// Code's screen and the list of agents under it, as five words:
//   - focus: list (its keys, "↑/↓ to select", in the box's status line's
//     place), box (the box's own status line drawn), other;
//   - viewed: main ("● main"), sub ("◯ main": a subagent's conversation
//     shown), none (no list);
//   - cursor, while the list has the keys: main or sub. Where its keys
//     say Enter would show another, the cursor is on a row not shown (◯),
//     the one such with a ❯ (a ❯ is also left where it was last clicked:
//     more than one, "many", not told apart); where they do not, it is on
//     the one shown (●). none: the list has not the keys;
//   - view: 1 where the list's keys say Enter would show another;
//   - empty: 1 where the box holds nothing but its placeholder ("Message
//     @…", or none).
//
// No box: "none none none 0 0".
var claudeFootAwk = strings.ReplaceAll(`function isrule(s) { sub(/^[ \t]*/, "", s); return index(s, "────────") == 1 || index(s, "━━━━━━━━") == 1 }
function isprompt(s) { sub(/^[ \t]*/, "", s); return index(s, "❯") == 1 }
function squeeze(s) { gsub(/[ \t]/, "", s); gsub(/NBSP/, "", s); return s }
function agentrow(s) { return s ~ /^[ \t]*(❯[ \t]+)?(◯|●) [^ \t]+([ \t][ \t]+.*|[ \t]*)$/ }
function mainrow(s) { return s ~ /^[ \t]*(❯[ \t]+)?(◯|●) main[ \t]*$/ }
function placeholder(i, e,  j, t, r) { r = ""; for (j = i; j < e; j++) { t = L[j]; if (j == i) sub(/^[ \t]*❯/, "", t); sub(/^([ \t]|NBSP)+/, "", t); sub(/([ \t]|NBSP)+$/, "", t); if (t != "") r = (r == "" ? t : r " " t) }; return r ~ /^Message @[A-Za-z0-9_.:-]+(…)?…$/ }
function modeline(s,  t) { t = s; sub(/^[ \tNBSP]*/, "", t); return index(t, "⏵") == 1 || index(t, "⏸") == 1 || index(squeeze(s), "?forshortcuts") == 1 }
{ sub(/\r$/, ""); L[NR] = $0 }
END {
  n = NR
  while (n > 0 && squeeze(L[n]) == "") n--
  for (i = n; i >= 2; i--) {
    if (!isprompt(L[i]) || !isrule(L[i-1])) continue
    e = 0
    for (j = i + 1; j <= n; j++) if (isrule(L[j])) { e = j; break }
    if (!e) continue
    for (j = e + 1; j <= n; j++) A[j] = 0
    j = e + 1
    while (j <= n) {
      if (!agentrow(L[j])) { j++; continue }
      k = j; m = 0
      while (k <= n && agentrow(L[k])) { if (mainrow(L[k])) m = 1; k++ }
      if (m) for (x = j; x < k; x++) A[x] = 1
      j = k
    }
    kl = ""; seen = 0; vw = "none"; cm = 0; cs = 0
    for (j = e + 1; j <= n; j++) {
      if (A[j]) {
        seen = 1
        if (mainrow(L[j])) { if (L[j] ~ /●/) vw = "main"; else if (vw == "none") vw = "sub" }
        if (L[j] ~ /^[ \t]*❯[ \t]+◯/) { if (mainrow(L[j])) cm++; else cs++ }
        continue
      }
      if (!seen && squeeze(L[j]) != "") kl = L[j]
    }
    # (Of the line above the list only, by how it begins: see ownerKeysLine.)
    if (!seen) kl = ""
    q = squeeze(kl)
    fo = "other"
    if (index(q, "↑/↓toselect") == 1) fo = "list"; else if (modeline(kl)) fo = "box"
    ev = (fo == "list" && index(q, "Entertoview")) ? 1 : 0
    cu = "none"
    if (fo == "list" && !ev) cu = vw
    else if (fo == "list") { if (cm + cs > 1) cu = "many"; else if (cm) cu = "main"; else if (cs) cu = "sub" }
    b = ""
    for (j = i; j < e; j++) { t = L[j]; if (j == i) sub(/^[ \t]*❯/, "", t); b = b squeeze(t) }
    # (The placeholder whole: see ownerPlaceholder.)
    em = (b == "" || placeholder(i, e)) ? 1 : 0
    print fo, vw, cu, ev, em; exit
  }
  print "none none none 0 0"
}`, "NBSP", " ")

// viewRestored: the gate's output without its first line saying it
// brought the conversation's own box back in front (a subagent's
// conversation had been shown: owned and back in convoGated), and whether
// it did.
func viewRestored(out []byte) ([]byte, bool) {
	return bytes.CutPrefix(out, []byte("\x01corral: main again\n"))
}

// The gate's exits for who has the box (convoGated: 13–15 before anything
// is typed, 16 typed and Enter held back).
const (
	exitOwnerSubagent = 13
	exitOwnerOther    = 14
	exitOwnerNone     = 15
	exitOwnerTyped    = 16
	// (Keys pressed to bring the conversation's box back in front, and
	// where that left the terminal not known: nothing of the message typed.)
	exitOwnerMoved = 17
	exitOwnerLast  = exitOwnerMoved
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
	case exitOwnerMoved:
		return "view_unsure", "the terminal was being brought back to this conversation when that stopped: nothing was typed; look at the terminal", false, true
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
