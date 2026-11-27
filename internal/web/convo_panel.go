package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// A panel of Claude Code's own (/rewind, /resume, /mcp, /config…) is drawn on
// its screen and tells the log nothing till it is shut, so the conversation
// shows it as the screen does and passes on the keys pressed: only while one
// is still open: its session file says so, a moment late, and its input box is
// not back on the screen, which is at once (the gate "panel"; an Escape past
// its panel would stop what it does then). A key that settles something (Enter,
// Space, a digit: /config's flips a setting at once) goes only onto the screen
// the person was shown, "panel:<digest>".
//
// The same window shows the terminal as it is where the page cannot make out
// what is up there or was refused a message: "look" and "look:<digest>", which
// ask nothing of its state, a key there being as one in the terminal, and no
// more than that; one that settles something goes only on the screen shown,
// what moves by itself aside (lookDigest). No key goes unless pressed; this does
// not resize the pane or bring it in front, as the terminal page does.

// panelKeyOK: what may be pressed in a panel: moving, confirming, cancelling,
// and the letters its search takes (not words pasted: a list takes them key by key).
var panelLetter = regexp.MustCompile(`^[A-Za-z0-9._/]$`)

func panelKeyOK(k string) bool {
	return replyKeys[k] || k == "shift+tab" || k == "backspace" || panelLetter.MatchString(k)
}

// panelSettles: a key whose effect depends on what is chosen on the screen.
func panelSettles(k string) bool {
	// (Shift+Tab: under a plan, it approves.)
	return k == "enter" || k == "space" || k == "shift+tab" || (len(k) == 1 && k[0] >= '0' && k[0] <= '9')
}

// panelOK: the one key pressed, and the digest it goes with where it settles something.
func panelOK(keys []string, digest string) bool {
	if len(keys) != 1 || !panelKeyOK(keys[0]) {
		return false
	}
	return !panelSettles(keys[0]) || hexDigest.MatchString(digest)
}

func (s *Server) convoPanelScreen(w http.ResponseWriter, ctx context.Context, sess *term.Session, agent, pane, socket string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := convoColorScreenCommand(sess.Persistence, pane, socket)
	if agent == "agy" {
		cmd = agyColorScreenCommand(sess.Persistence, pane, socket)
	}
	res, err := sshx.Exec(ctx, sess.Client(), cmd, 512<<10, sshx.ExecOptions{Release: release})
	if err != nil {
		// (Given up on by the page, a key having come: nothing to say of it, and
		// nothing to note — it is no failure to read.)
		if ctx.Err() != nil {
			*outcome, *reason = "unknown", "cancelled"
			return
		}
		if errors.Is(err, sshx.ErrBusy) {
			*outcome, *reason = "refused", "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		}
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	*outcome = "looked"
	// (What is shown and what names it, of the one read: read twice they could
	// be of two screens, the choice shown not the one Enter would take. The
	// gate reads it the same way, colours and all.)
	text, _, _ := screenOf(res.Out)
	digest := panelDigest(res.Out)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "digest": digest, "look": lookDigest(res.Out)})
}

// herdrReadPanelANSI reads the pane with ANSI colours directly over herdr's
// Unix socket (~1ms on host, zero shell spawn). Each herdr RPC uses its own
// short-lived connection because herdr closes a Unix socket connection after
// answering one request. For agy, prefers "recent" (last 60 lines) so
// scrolled-up panes still show the live prompt at the bottom.
func herdrReadPanelANSI(ctx context.Context, sess *term.Session, socket, pane string, agy bool) (raw []byte, dialed bool, err error) {
	read := func(source string, lines int) (string, bool, error) {
		c, derr := herdr.DialWait(ctx, sess.Client(), socket, 2*time.Second)
		if derr != nil {
			return "", false, derr
		}
		defer c.Close()
		text, rerr := c.ReadPaneANSI(ctx, pane, source, lines)
		return text, true, rerr
	}
	if agy {
		text, ok, rerr := read("recent", 60)
		if rerr == nil {
			return []byte(text), true, nil
		}
		if !ok {
			return nil, false, rerr
		}
	}
	text, ok, rerr := read("visible", 0)
	if rerr != nil {
		return nil, ok, rerr
	}
	return []byte(text), true, nil
}

// convoHerdrPanelScreen serves panel_screen directly over herdr's Unix socket,
// avoiding the full-host /proc scan and SSH shell spawn on every poll.
// Returns false when the socket cannot be reached so the caller can fall back.
func (s *Server) convoHerdrPanelScreen(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, agy bool, outcome, reason *string, release func()) bool {
	raw, dialed, err := herdrReadPanelANSI(ctx, sess, socket, pane, agy)
	if !dialed {
		return false
	}
	defer release()
	if err != nil {
		if ctx.Err() != nil {
			*outcome, *reason = "unknown", "cancelled"
			return true
		}
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return true
	}
	*outcome = "looked"
	text, _, _ := screenOf(raw)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "digest": panelDigest(raw), "look": lookDigest(raw)})
	return true
}

// convoHerdrPanelKey serves live PanelWindow keypresses directly over herdr's
// Unix socket: optional lookDigest check, send key, brief repaint pause, and
// immediate ANSI screen read (each RPC on its own short-lived connection
// because herdr closes a connection after one request).
// Returns false if the socket cannot be reached so the caller can fall back.
func (s *Server) convoHerdrPanelKey(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, agy, codex bool, key, digest string, outcome, reason *string, release func()) bool {
	needDigest := panelSettles(key) || (codex && panelLetter.MatchString(key))
	if needDigest {
		raw, dialed, err := herdrReadPanelANSI(ctx, sess, socket, pane, agy)
		if !dialed {
			return false
		}
		if err != nil {
			release()
			*reason = "no_screen"
			writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
			return true
		}
		if lookDigest(raw) != digest {
			release()
			*outcome, *reason = "refused", "screen_changed"
			writeError(w, http.StatusConflict, "screen_changed", "the screen changed; look again before answering")
			return true
		}
	}
	c, err := herdr.DialWait(ctx, sess.Client(), socket, 2*time.Second)
	if err != nil {
		if needDigest {
			release()
			*outcome, *reason = "refused", "send_failed"
			writeError(w, http.StatusBadGateway, "send_failed", "could not send key to pane")
			return true
		}
		return false
	}
	defer release()
	var serr error
	if key == "shift+tab" {
		serr = c.SendInput(ctx, pane, "\x1b[Z", nil)
	} else {
		serr = c.SendKeys(ctx, pane, []string{key})
	}
	c.Close()
	if serr != nil {
		*outcome, *reason = "refused", "send_failed"
		writeError(w, http.StatusBadGateway, "send_failed", "could not send key to pane")
		return true
	}
	*outcome = "typed"
	// Brief pause so the TUI processes the key before reading the updated screen.
	select {
	case <-ctx.Done():
	case <-time.After(60 * time.Millisecond):
	}
	out := map[string]any{"ok": true}
	if raw, _, err := herdrReadPanelANSI(ctx, sess, socket, pane, agy); err == nil {
		text, _, _ := screenOf(raw)
		out["text"], out["digest"], out["look"] = text, panelDigest(raw), lookDigest(raw)
	}
	writeJSON(w, http.StatusOK, out)
	return true
}

// panelDigest names a screen as read with its colours, without them: a cursor
// blinking in a panel's search box changes only its colours, and would never
// let a key that goes with the screen through. Read off that one read, so that
// it is of what is shown. The gate computes the same on the host: panelDigestPipe
// (cross-checked in a test).
func panelDigest(raw []byte) string {
	sum := sha256.Sum256(steadyScreen(panelSGR.ReplaceAll(bytes.ReplaceAll(raw, []byte{0}, nil), nil)))
	return hex.EncodeToString(sum[:])
}

var panelSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// panelDigestPipe: panelDigest as a shell pipeline, of what comes in.
const panelDigestPipe = `tr -d '\000' | LC_ALL=C sed "s/$(printf '\033')\[[0-9;:]*m//g" | LC_ALL=C awk '` + steadyAwk + `' | sha256sum`

// A tool's row under way blinks: its "●" (macOS's "⏺") goes to a blank and back
// a few times a second, on a screen where nothing else moves. Counted, the
// digest went between two values, and half of all answers to the prompt under
// it were refused as "the screen changed" (11-06). Likewise, when background
// tasks are running, Claude Code ticks a spinner above and background-agent
// timers / status chrome below; for an active AskUserQuestion or Review dialog
// at the bottom of the screen, only the dialog itself (tab bar, question or
// review summary, options, ticks, and ❯ cursor) is hashed. Both sides — Go's
// steadyScreen and the host's steadyPipe — compute the exact same bytes.
var (
	blinkingMark = regexp.MustCompile(`(?m)^([ \t]*)(?:●|⏺)`)
	askPureRule  = regexp.MustCompile(`^[ \t]*[─━╌▔]{8,}[ \t]*$`)
	askHintRule  = regexp.MustCompile(`^[ \t]*[─━╌▔]{2,}.*[─━╌▔]{2,}[ \t]*$`)
	askMultiTab  = regexp.MustCompile(`^[ \t]*(?:←[ \t]+)?(?:☐|☒|☑)[ \t]+[^ \t].*[ \t]+(?:✔|✓)[ \t]+Submit(?:[ \t]+→)?[ \t]*$`)
	askLoneTab   = regexp.MustCompile(`^[ \t]*(?:☐|☒)[ \t]+[^ \t]`)
	askOptLine   = regexp.MustCompile(`^[ \t]*(❯)?[ \t]*([0-9]{1,2})\.[ \t]+(.*)$`)
	askSubmitLn  = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?Submit[ \t]*$`)
	askChatLine  = regexp.MustCompile(`^[ \t]*(?:[0-9]{1,2}\.[ \t]+)?Chat about this[ \t]*$`)
	askReviewHd  = regexp.MustCompile(`^[ \t]*Review your answers[ \t]*$`)
	askHintSeg   = regexp.MustCompile(`^(?:Enter to (?:select|confirm|submit)|(?:[↑↓←→/]+|Tab/Arrow keys|Arrow keys) to navigate|Tab to (?:switch questions|amend)|n to add notes|Esc to cancel|shift\+tab to approve with this feedback|ctrl\+g to edit in Vim(?: .*)?)$`)
	askNavSeg    = regexp.MustCompile(`Enter to (?:select|confirm|submit)|to navigate|Tab to switch questions`)
	bgFooterRe   = regexp.MustCompile(`[0-9]+ background (?:task|agent|shell)s?|[0-9]+ local agents?|↓ to manage|ctrl\+o to expand|ctrl\+t to hide`)
	bgStatusRe   = regexp.MustCompile(`│.*(?:Ctx |\$[0-9])|(?:Ctx |\$[0-9]).*│`)
	bgModeRe     = regexp.MustCompile(`^[ \t]*(?:⏵|⏸|\? for shortcuts|Tip:[ \t])`)
)

func isAskRule(s string) bool {
	return askPureRule.MatchString(s) || (askHintRule.MatchString(s) && !bytes.Contains([]byte(s), []byte("❯")))
}

func isAskHintLine(s string) bool {
	t := strings.Trim(s, " \t")
	if t == "" || strings.Contains(t, "❯") {
		return false
	}
	if strings.HasPrefix(t, "ctrl+g to edit in Vim") && (len(t) == len("ctrl+g to edit in Vim") || strings.HasPrefix(t[len("ctrl+g to edit in Vim"):], " · ")) {
		return true
	}
	parts := strings.Split(t, "·")
	n := 0
	for _, p := range parts {
		p = strings.Trim(p, " \t")
		if p == "" {
			continue
		}
		if !askHintSeg.MatchString(p) {
			return false
		}
		n++
	}
	return n > 0
}

func isBgFooterLine(s string) bool {
	if strings.Contains(s, "❯") || askOptLine.MatchString(s) {
		return false
	}
	return bgModeRe.MatchString(s) || bgFooterRe.MatchString(s) || bgStatusRe.MatchString(s)
}

func isIndented(s string) bool {
	return strings.HasPrefix(s, "  ") || strings.HasPrefix(s, "\t")
}

// askDialogBounds returns the [top, end] (0-indexed inclusive) line range of an
// active Claude Code AskUserQuestion or Review your answers dialog at the
// bottom of lines (each with trailing \r stripped). Returns ok=false for any
// other screen (e.g. a permission prompt or stale scrollback under another
// dialog).
func askDialogBounds(lines []string) (top, end int, ok bool) {
	n := len(lines)
	for n > 0 && blankLine.MatchString(lines[n-1]) {
		n--
	}
	if n < 2 {
		return 0, 0, false
	}
	k := n - 1
	sawAskHint, sawChat := false, false
	for k >= 0 {
		l := lines[k]
		if blankLine.MatchString(l) {
			k--
			continue
		}
		if agentRow.MatchString(l) {
			j, hasMain := k, false
			for j >= 0 && agentRow.MatchString(lines[j]) {
				hasMain = hasMain || agentMain.MatchString(lines[j])
				j--
			}
			if hasMain {
				k = j
				continue
			}
			break
		}
		if isAskHintLine(l) {
			if askNavSeg.MatchString(l) {
				sawAskHint = true
			}
			k--
			continue
		}
		if k >= 1 && !strings.Contains(l, "❯") && !askOptLine.MatchString(l) && !blankLine.MatchString(lines[k-1]) && !strings.Contains(lines[k-1], "❯") && !askOptLine.MatchString(lines[k-1]) {
			prev := strings.Trim(lines[k-1], " \t")
			cur := strings.Trim(l, " \t")
			if joined := prev + " " + cur; isAskHintLine(joined) {
				if askNavSeg.MatchString(joined) {
					sawAskHint = true
				}
				k -= 2
				continue
			}
			if joined := prev + cur; isAskHintLine(joined) {
				if askNavSeg.MatchString(joined) {
					sawAskHint = true
				}
				k -= 2
				continue
			}
		}
		if isAskRule(l) {
			k--
			continue
		}
		if askChatLine.MatchString(l) {
			sawChat = true
			k--
			continue
		}
		if spinnerLine.MatchString(l) || isBgFooterLine(l) {
			k--
			continue
		}
		break
	}
	end = k
	if end < 1 {
		return 0, 0, false
	}
	submitCursor := 0
	scanStart := end
	if askSubmitLn.MatchString(lines[end]) {
		if strings.Contains(lines[end], "❯") {
			submitCursor = 1
		}
		scanStart = end - 1
	}
	optCount, lastNum, cursorCount, firstOpt, maxOptLine := 0, 0, 0, -1, -1
	opt1Text, opt2Text := "", ""
	badOpt := false
	limit := scanStart - 80
	if limit < 0 {
		limit = 0
	}
	for i := scanStart; i >= limit; i-- {
		l := lines[i]
		if isAskRule(l) || askMultiTab.MatchString(l) || askLoneTab.MatchString(l) || askReviewHd.MatchString(l) {
			break
		}
		if m := askOptLine.FindStringSubmatch(l); m != nil {
			num := 0
			for _, ch := range m[2] {
				num = num*10 + int(ch-'0')
			}
			if optCount == 0 {
				lastNum = num
				maxOptLine = i
			} else if num != lastNum-1 {
				badOpt = true
				break
			} else {
				lastNum = num
			}
			optCount++
			if m[1] != "" {
				cursorCount++
			}
			if num == 1 {
				opt1Text = strings.Trim(m[3], " \t")
			} else if num == 2 {
				opt2Text = strings.Trim(m[3], " \t")
			}
			firstOpt = i
			if num == 1 {
				break
			}
		} else if strings.Contains(l, "❯") {
			badOpt = true
			break
		}
	}
	if !badOpt && maxOptLine >= 0 {
		seenBlank := false
		for m := maxOptLine + 1; m <= end; m++ {
			if askSubmitLn.MatchString(lines[m]) {
				continue
			}
			if blankLine.MatchString(lines[m]) {
				seenBlank = true
				continue
			}
			if seenBlank && !isIndented(lines[m]) {
				badOpt = true
				break
			}
		}
	}
	if badOpt || optCount < 2 || lastNum != 1 || cursorCount+submitCursor != 1 || firstOpt <= 0 {
		return 0, 0, false
	}
	// (A) Review your answers dialog.
	if optCount == 2 && opt1Text == "Submit answers" && opt2Text == "Cancel" {
		revLimit := firstOpt - 60
		if revLimit < 0 {
			revLimit = 0
		}
		revIdx := -1
		for j := firstOpt - 1; j >= revLimit; j-- {
			l := lines[j]
			if isAskRule(l) || askMultiTab.MatchString(l) {
				break
			}
			if askReviewHd.MatchString(l) {
				revIdx = j
			}
		}
		if revIdx >= 0 {
			top = revIdx
			for u := revIdx - 1; u >= 0; u-- {
				if blankLine.MatchString(lines[u]) {
					continue
				}
				if askMultiTab.MatchString(lines[u]) {
					top = u
				}
				break
			}
			return top, end, true
		}
	}
	// (B) AskUserQuestion dialog: anchor the tab bar strictly at the first
	// non-blank line under the dialog's top border rule (or at the very top of
	// the screen when the rule scrolled off) so a tab-like line inside the
	// question body (e.g. "☐ Preview ✔ Submit") can never drop earlier lines
	// of the question from the digest.
	if !sawAskHint && !sawChat {
		return 0, 0, false
	}
	tabLimit := firstOpt - 60
	if tabLimit < 0 {
		tabLimit = 0
	}
	firstNonBlank, nonBlankCount := -1, 0
	hitRule := false
	for j := firstOpt - 1; j >= tabLimit; j-- {
		l := lines[j]
		if isAskRule(l) {
			hitRule = true
			break
		}
		if !blankLine.MatchString(l) {
			firstNonBlank = j
			nonBlankCount++
		}
	}
	if (hitRule || tabLimit == 0) && firstNonBlank >= 0 && nonBlankCount >= 2 &&
		(askMultiTab.MatchString(lines[firstNonBlank]) || askLoneTab.MatchString(lines[firstNonBlank])) {
		return firstNonBlank, end, true
	}
	return 0, 0, false
}

// steadyScreen: a screen as it is named: no NUL, each line ended; for an active
// AskUserQuestion or Review dialog, only the dialog itself (tab bar, question,
// options, ticks, and cursor); otherwise the whole screen with live spinner
// lines, bottom agent-list row ages, and blinking leading marks normalized.
func steadyScreen(raw []byte) []byte {
	b := bytes.ReplaceAll(raw, []byte{0}, nil)
	if len(b) == 0 {
		return b
	}
	trimmed := bytes.TrimSuffix(b, []byte("\n"))
	rawLines := bytes.Split(trimmed, []byte("\n"))
	plain := make([]string, len(rawLines))
	for i, rl := range rawLines {
		plain[i] = strings.TrimSuffix(string(rl), "\r")
	}
	if top, end, ok := askDialogBounds(plain); ok {
		var out bytes.Buffer
		for i := top; i <= end; i++ {
			out.Write(rawLines[i])
			out.WriteByte('\n')
		}
		return out.Bytes()
	}
	out := agentRowsAged(append(bytes.Join(rawLines, []byte("\n")), '\n'))
	out = spinnerLine.ReplaceAll(out, nil)
	out = blinkingMark.ReplaceAll(out, []byte("${1} "))
	return out
}

// (One mark a line, as above: "t" goes on to the next line once one is taken out.)
const blinkSed = `LC_ALL=C sed -e 's/^\([ 	]*\)●/\1 /' -e 't' -e 's/^\([ 	]*\)⏺/\1 /'`

// steadyAwk is steadyScreen as a POSIX awk program (bytes: LC_ALL=C; works on
// awk, mawk, gawk; no single quotes).
const steadyAwk = `function isblank(s) { return s ~ /^[ \t]*$/ }
function isrule(s) {
  if (s ~ /^[ \t]*(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)+[ \t]*$/) return 1
  return index(s, "\342\235\257") == 0 && s ~ /^[ \t]*(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224).*(\342\224\200|\342\224\201|\342\225\214|\342\226\224)(\342\224\200|\342\224\201|\342\225\214|\342\226\224)[ \t]*$/
}
function ismultitab(s) { return s ~ /^[ \t]*(\342\206\220[ \t]+)?(\342\230\220|\342\230\222|\342\230\221)[ \t]+[^ \t].*[ \t]+(\342\234\224|\342\234\223)[ \t]+Submit([ \t]+\342\206\222)?[ \t]*$/ }
function islonetab(s) { return s ~ /^[ \t]*(\342\230\220|\342\230\222)[ \t]+[^ \t]/ }
function isopt(s) { return s ~ /^[ \t]*(\342\235\257)?[ \t]*[0-9][0-9]?\.[ \t]+/ }
function optnum(s,  t) { t = s; sub(/^[ \t]*(\342\235\257)?[ \t]*/, "", t); sub(/\..*$/, "", t); return t + 0 }
function optrest(s,  t) { t = s; sub(/^[ \t]*(\342\235\257)?[ \t]*[0-9][0-9]?\.[ \t]+/, "", t); sub(/[ \t]+$/, "", t); return t }
function optcur(s) { return s ~ /^[ \t]*\342\235\257[ \t]*[0-9][0-9]?\.[ \t]+/ }
function issubmit(s) { return s ~ /^[ \t]*(\342\235\257[ \t]+)?Submit[ \t]*$/ }
function ischat(s) { return s ~ /^[ \t]*([0-9][0-9]?\.[ \t]+)?Chat about this[ \t]*$/ }
function isrevhead(s) { return s ~ /^[ \t]*Review your answers[ \t]*$/ }
function isagentrow(s) { return s ~ /^[ \t]*(\342\235\257[ \t]+)?(\342\227\217|\342\227\257)[ \t]/ }
function isagentmain(s) { return s ~ /^[ \t]*(\342\235\257[ \t]+)?(\342\227\217|\342\227\257)[ \t]+main[ \t]*$/ }
function isspinner(s) { return s ~ /^(\*|\302\267|\342\234\242|\342\234\263|\342\234\266|\342\234\273|\342\234\275) ([A-Za-z0-9 -]*\342\200\246.*|Waiting for [0-9]+ background (task|agent|shell)s?.*)$/ }
function ishintseg(p) {
  return p ~ /^(Enter to (select|confirm|submit)|((\342\206\221|\342\206\223|\342\206\220|\342\206\222|\/)+|Tab\/Arrow keys|Arrow keys) to navigate|Tab to (switch questions|amend)|n to add notes|Esc to cancel|shift\+tab to approve with this feedback|ctrl\+g to edit in Vim( .*)?)$/
}
function ishint(s,  t, parts, np, i, p, c) {
  t = s; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t)
  if (t == "" || index(t, "\342\235\257") > 0) return 0
  if (t == "ctrl+g to edit in Vim" || index(t, "ctrl+g to edit in Vim \302\267 ") == 1) return 1
  np = split(t, parts, "\302\267")
  c = 0
  for (i = 1; i <= np; i++) {
    p = parts[i]; sub(/^[ \t]+/, "", p); sub(/[ \t]+$/, "", p)
    if (p == "") continue
    if (!ishintseg(p)) return 0
    c++
  }
  return c > 0
}
function isnavhint(s) { return s ~ /(Enter to (select|confirm|submit)|to navigate|Tab to switch questions)/ }
function isbgfoot(s) {
  if (index(s, "\342\235\257") > 0 || isopt(s)) return 0
  if (s ~ /^[ \t]*(\342\217\265|\342\217\270|\? for shortcuts|Tip:[ \t])/) return 1
  if (s ~ /([0-9]+ background (task|agent|shell)s?|[0-9]+ local agents?|\342\206\223 to manage|ctrl\+o to expand|ctrl\+t to hide)/) return 1
  if (s ~ /(\342\224\202.*(Ctx |\$[0-9])|(Ctx |\$[0-9]).*\342\224\202)/) return 1
  return 0
}
function isindented(s) { return substr(s, 1, 2) == "  " || substr(s, 1, 1) == "\t" }
function trimws(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
{ raw[NR] = $0; p = $0; sub(/\r$/, "", p); L[NR] = p }
END {
  if (NR == 0) exit
  n = NR
  while (n > 0 && isblank(L[n])) n--
  top = 0; end = 0
  if (n >= 2) {
    k = n; sawAsk = 0; sawChat = 0
    while (k >= 1) {
      l = L[k]
      if (isblank(l)) { k--; continue }
      if (isagentrow(l)) {
        j = k; m = 0
        while (j >= 1 && isagentrow(L[j])) { if (isagentmain(L[j])) m = 1; j-- }
        if (m) { k = j; continue }
        break
      }
      if (ishint(l)) { if (isnavhint(l)) sawAsk = 1; k--; continue }
      if (k >= 2 && index(l, "\342\235\257") == 0 && !isopt(l) && !isblank(L[k-1]) && index(L[k-1], "\342\235\257") == 0 && !isopt(L[k-1])) {
        js = trimws(L[k-1]) " " trimws(l)
        if (ishint(js)) { if (isnavhint(js)) sawAsk = 1; k -= 2; continue }
        jt = trimws(L[k-1]) trimws(l)
        if (ishint(jt)) { if (isnavhint(jt)) sawAsk = 1; k -= 2; continue }
      }
      if (isrule(l)) { k--; continue }
      if (ischat(l)) { sawChat = 1; k--; continue }
      if (isspinner(l) || isbgfoot(l)) { k--; continue }
      break
    }
    if (k >= 2) {
      end = k
      subcur = 0; ss = end
      if (issubmit(L[end])) { if (index(L[end], "\342\235\257") > 0) subcur = 1; ss = end - 1 }
      oc = 0; lnum = 0; ccnt = 0; fopt = 0; maxopt = 0; o1 = ""; o2 = ""; bad = 0
      lim = ss - 80; if (lim < 1) lim = 1
      for (i = ss; i >= lim; i--) {
        l = L[i]
        if (isrule(l) || ismultitab(l) || islonetab(l) || isrevhead(l)) break
        if (isopt(l)) {
          num = optnum(l)
          if (oc == 0) { lnum = num; maxopt = i }
          else if (num != lnum - 1) { bad = 1; break }
          else lnum = num
          oc++
          if (optcur(l)) ccnt++
          if (num == 1) o1 = optrest(l)
          else if (num == 2) o2 = optrest(l)
          fopt = i
          if (num == 1) break
        } else if (index(l, "\342\235\257") > 0) { bad = 1; break }
      }
      if (!bad && maxopt > 0) {
        sb = 0
        for (m = maxopt + 1; m <= end; m++) {
          if (issubmit(L[m])) continue
          if (isblank(L[m])) { sb = 1; continue }
          if (sb && !isindented(L[m])) { bad = 1; break }
        }
      }
      if (!bad && oc >= 2 && lnum == 1 && ccnt + subcur == 1 && fopt > 1) {
        if (oc == 2 && o1 == "Submit answers" && o2 == "Cancel") {
          rlim = fopt - 60; if (rlim < 1) rlim = 1
          ridx = 0
          for (j = fopt - 1; j >= rlim; j--) {
            if (isrule(L[j]) || ismultitab(L[j])) break
            if (isrevhead(L[j])) ridx = j
          }
          if (ridx > 0) {
            top = ridx
            for (u = ridx - 1; u >= 1; u--) {
              if (isblank(L[u])) continue
              if (ismultitab(L[u])) top = u
              break
            }
          }
        }
        if (!top && (sawAsk || sawChat)) {
          tlim = fopt - 60; if (tlim < 1) tlim = 1
          fnb = 0; nbc = 0; hitrule = 0
          for (j = fopt - 1; j >= tlim; j--) {
            l = L[j]
            if (isrule(l)) { hitrule = 1; break }
            if (!isblank(l)) { fnb = j; nbc++ }
          }
          if ((hitrule || tlim == 1) && fnb > 0 && nbc >= 2 && (ismultitab(L[fnb]) || islonetab(L[fnb]))) top = fnb
        }
      }
    }
  }
  if (top > 0) {
    for (i = top; i <= end; i++) print raw[i]
    exit
  }
  # Fallback: full screen with bottom agent-list row ages, spinner lines, and blinking marks normalized.
  an = NR
  while (an > 0 && isblank(raw[an])) an--
  ai = an; am = 0
  while (ai > 0 && isagentrow(raw[ai])) { if (isagentmain(raw[ai])) am = 1; ai-- }
  if (am) for (k = ai + 1; k <= an; k++) while (sub(/[ \t]+[0-9]+[hms][ \t]*$/, "", raw[k])) ;
  for (i = 1; i <= NR; i++) {
    s = raw[i]
    if (isspinner(s)) s = ""
    else if (match(s, /^[ \t]*(\342\227\217|\342\217\272)/)) s = substr(s, 1, RLENGTH - 3) " " substr(s, RLENGTH + 1)
    print s
  }
}`

const steadyPipe = `tr -d '\000' | LC_ALL=C awk '` + steadyAwk + `'`

// The terminal shown as it is names its screen without what is seen to move by
// itself: an agent at work draws a spinner and counts seconds, and a key that had
// to go on that very screen could never go. Only that goes, each seen on real
// screens: the spinner's own line, the mark that blinks, and the seconds at the
// end of the rows of the list of agents: the last thing on the screen, with its
// "main" row (words of a prompt that look like a row are neither). Everything
// else counts, digits and blanks too: "kill -TERM 1234" is not "kill -TERM 5678".
// Anything else moving: the screen is said to have changed, nothing is pressed.
var spinnerMarks = []string{`*`, `·`, `✢`, `✳`, `✶`, `✻`, `✽`}

var (
	spinnerLine = regexp.MustCompile(`(?m)^(?:\*|·|✢|✳|✶|✻|✽) (?:[A-Za-z0-9 -]*….*|Waiting for [0-9]+ background (?:task|agent|shell)s?.*)$`)
	agentRow    = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?(?:●|◯)[ \t]`)
	agentMain   = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?(?:●|◯)[ \t]+main[ \t]*$`)
	rowAge      = regexp.MustCompile(`[ \t]+[0-9]+[hms][ \t]*$`)
	blankLine   = regexp.MustCompile(`^[ \t]*$`)
)

// agentRowsAged: b, its lines ended, without the time at the end of the rows of
// a list of agents at its end.
func agentRowsAged(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	n := len(lines)
	for n > 0 && blankLine.Match(lines[n-1]) {
		n--
	}
	i, hasMain := n, false
	for i > 0 && agentRow.Match(lines[i-1]) {
		i--
		hasMain = hasMain || agentMain.Match(lines[i])
	}
	if hasMain {
		for k := i; k < n; k++ {
			for rowAge.Match(lines[k]) {
				lines[k] = rowAge.ReplaceAll(lines[k], nil)
			}
		}
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

const agentRowsAwk = `{ l[NR] = $0 } END { n = NR; while (n > 0 && l[n] ~ /^[ 	]*$/) n--; i = n; m = 0; while (i > 0 && l[i] ~ /^[ 	]*(❯[ 	]+)?(●|◯)[ 	]/) { if (l[i] ~ /^[ 	]*(❯[ 	]+)?(●|◯)[ 	]+main[ 	]*$/) m = 1; i-- } if (m) for (k = i + 1; k <= n; k++) while (sub(/[ 	]+[0-9]+[hms][ 	]*$/, "", l[k])) ; for (k = 1; k <= NR; k++) print l[k] }`

// OpenCode 2's, at work: the spinner before a command it runs ("┃  ⠼ touch x",
// one of the Braille patterns, turning while it asks leave to run it too) and
// the bar at its foot ("⬝⬝■■■■■■ esc interrupt"), each the same for the name.
// Seen on real screens (OpenCode 2.2.6, 11-08); nothing else of its.
var (
	ocSpinner = regexp.MustCompile(`(?m)^([ \t]*┃[ \t]+)[\x{2800}-\x{28FF}]`)
	ocBar     = regexp.MustCompile(`(?m)^([ \t]*)[⬝■]+ `)
)

// ocMotionAwk: ocSpinner and ocBar on the host (bytes: LC_ALL=C).
const ocMotionAwk = `{ if (match($0, /^[ 	]*\342\224\203[ 	]+\342[\240-\243][\200-\277]/)) $0 = substr($0, 1, RLENGTH - 3) " " substr($0, RLENGTH + 1)
  else if (match($0, /^[ 	]*(\342\254\235|\342\226\240)+ /)) { e = RLENGTH; match($0, /^[ 	]*/); $0 = substr($0, 1, RLENGTH) "\342\226\240 " substr($0, e + 1) }
  print }`

func lookDigest(raw []byte) string {
	b := panelSGR.ReplaceAll(bytes.ReplaceAll(raw, []byte{0}, nil), nil)
	b = ocBar.ReplaceAll(ocSpinner.ReplaceAll(b, []byte("${1} ")), []byte("${1}■ "))
	b = blinkingMark.ReplaceAll(spinnerLine.ReplaceAll(agentRowsAged(b), nil), []byte("${1} "))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lookDigestPipe: lookDigest as a shell pipeline, of what comes in.
var lookDigestPipe = func() string {
	sed := ""
	for _, m := range spinnerMarks {
		if m == "*" {
			m = `\*`
		}
		sed += ` -e 's/^` + m + ` [A-Za-z0-9 -]*….*$//'`
		sed += ` -e 's/^` + m + ` Waiting for [0-9][0-9]* background task.*$//'`
		sed += ` -e 's/^` + m + ` Waiting for [0-9][0-9]* background agent.*$//'`
		sed += ` -e 's/^` + m + ` Waiting for [0-9][0-9]* background shell.*$//'`
	}
	return `tr -d '\000' | LC_ALL=C sed "s/$(printf '\033')\[[0-9;:]*m//g" | LC_ALL=C awk '` + ocMotionAwk + `' | LC_ALL=C awk '` + agentRowsAwk + `' | LC_ALL=C sed` + sed + ` | ` + blinkSed + ` | sha256sum`
}()

// POST /api/sessions/{id}/conversation/panel-screen → {"text", "digest"}: the
// screen, with its colours, to show a panel of its own by.
func (s *Server) apiConversationPanelScreen(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "panel_screen")
}

// POST /api/sessions/{id}/conversation/panel (as a press, with one key, and
// the digest of the screen shown where the key settles something); 409
// no_dialog where none is open any more, screen_changed where it is not the
// one shown: nothing pressed.
func (s *Server) apiConversationPanel(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "panel")
}
