package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// agyScreenCommand prints the live bottom of an agy pane (plain text). In
// herdr, --source recent --lines 60 reads the live tail even when the user
// scrolled up in the pane (whereas --source visible returns the scrolled-up
// viewport without the input box), falling back to --source visible if needed.
func agyScreenCommand(mode, pane, socket string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then tmux -u capture-pane -p -t "$2" 2>/dev/null 9>&-
else HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source recent --lines 60 2>/dev/null 9>&- || HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source visible 2>/dev/null 9>&-; fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(socket)
}

// agyColorScreenCommand is agyScreenCommand with ANSI SGR colours preserved,
// used to distinguish greyed placeholders/suggestions from user drafts and to
// show the live bottom in PanelWindow even if the herdr pane was scrolled up.
func agyColorScreenCommand(mode, pane, socket string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then tmux -u capture-pane -p -e -t "$2" 2>/dev/null 9>&-
else HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source recent --lines 60 --format ansi 2>/dev/null 9>&- || HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source visible --format ansi 2>/dev/null 9>&-; fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(socket)
}

// Antigravity CLI (agy) process and screen helpers.
//
// An interactive agy process holds open
// ~/.gemini/antigravity-cli/presence/<uuid>.lock for its active conversation
// (and switches that lock on /clear while older conversations/<uuid>.db FDs
// may remain open). Before its first message, no presence lock is open yet,
// so locate emits the zero session id (convoNoSession) and empty log.

// agyFdsSh: POSIX shell helpers for locating an agy process's active session
// and excluding non-interactive invocations (-p/--print/--prompt or utility
// subcommands). Included in convoLocateWith and convoGated.
const agyFdsSh = `
agy_noninter() {
  tr '\0' '\001' <"/proc/$1/cmdline" 2>/dev/null | awk 'BEGIN { RS = "\001" }
    NR == 1 { next }
    skip { skip = 0; next }
    /^--$/ { exit }
    /^(-p|--print|--prompt)$/ { print 1; exit }
    /^(--print=|--prompt=)/ { print 1; exit }
    /^(-i|--prompt-interactive|--add-dir|--agent|--conversation|--effort|--input-format|--json-schema|--log-file|--mode|--model|--output-format|--print-timeout|--project|--settings)$/ { skip = 1; next }
    /^-/ { next }
    /^(acp|agent|agents|changelog|help|install|mcp|mic-serve|models|plugin|plugins|remote-control|update)$/ { print 1; exit }
    { exit }'
}
agy_fds() {
  agysid= agylog= agybase=
  agynl=$(printf '\nx'); agynl=${agynl%x}
  for fd in "$1"/fd/*; do
    l=$(readlink "$fd" 2>/dev/null) || continue
    case $l in
    *"$agynl"*) continue;;
    */presence/????????-????-????-????-????????????.lock)
      id=${l##*/}; id=${id%.lock}
      case $id in *[!0-9a-fA-F-]*) continue;; esac
      agysid=$(printf '%s' "$id" | tr 'A-F' 'a-f')
      agybase=${l%/presence/*}
      cand="$agybase/brain/$agysid/.system_generated/logs/transcript_full.jsonl"
      [ -f "$cand" ] && agylog=$cand
      break ;;
    esac
  done
}
`

// errAgyAsks: agy is currently displaying a permission prompt, question, or
// trust dialog in place of its input box.
var errAgyAsks = errors.New("it asks something: its input is not there")

// agyAsking: phrases present in agy's permission, question, or trust prompts.
// Deliberately excludes "esc to cancel", which agy also prints on its working
// footer bar whenever a turn or command is running (including while showing
// the non-blocking "How's the CLI experience so far?" feedback strip).
var agyAsking = []string{
	"Requesting permission for:",
	"Run this command?",
	"Do you trust the contents of this project?",
	"↑/↓ Navigate",
	"tab Amend",
	"ctrl+g edit/expand",
	"ctrl+r Review",
	"esc Skip",
	"enter Confirm",
}

// agyChoiceCursorRe matches the active numbered option line of an interactive
// permission or question menu (e.g. "> 1. Yes, run command"), without matching
// the non-blocking "[1] Good [2] Fine [3] Bad [0] Skip" feedback strip.
var agyChoiceCursorRe = regexp.MustCompile(`(?m)^[ \t]*[>›❯][ \t]*\d+\.[ \t]+\S`)

// agyQuestionHeaderRe matches agy's multi-question header ("Question 1/2").
var agyQuestionHeaderRe = regexp.MustCompile(`(?m)^[ \t]*Question[ \t]+\d+/\d+`)

// agyRule: a horizontal rule drawn above and below agy's input box (≥12 box-drawing dashes).
var agyRule = regexp.MustCompile(`^[ \t]*─{12,}[ \t]*$`)

// agyPlaceholder: grey hint lines agy shows inside an empty input box across
// its modes (auto, accept-edits, plan, default).
var agyPlaceholder = regexp.MustCompile(`^(?:Auto mode:|Accept[- ]edits(?: mode)?:|Plan mode:|Manual mode:|Default mode:|Type a message|Ask anything|Press |Send a message|\s*$)`)

// agyIdleInput checks that agy's input box (two horizontal rules enclosing
// "> ...") is visible at the foot of a plain screen and holds no user draft.
// Returns errAgyAsks when a prompt/question replaces the box, errDraft when
// the box holds typed text, or errScreen when the layout is unrecognized.
func agyIdleInput(screen string) error {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	top, bot, ok := agyBoxBounds(lines)
	if !ok {
		return agyNotBoxedErr(lines)
	}
	var parts []string
	for i := top + 1; i < bot; i++ {
		t := strings.TrimSpace(lines[i])
		if i == top+1 {
			t = strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(lines[i], " \t"), ">"))
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	joined := strings.Join(parts, " ")
	if joined != "" && !agyPlaceholder.MatchString(joined) {
		return errDraft
	}
	return nil
}

// agyIdleInputColor is agyIdleInput on an ANSI-coloured screen: any grey/dim
// text inside the input box (SGR 2, 90, or 38;5;8 — agy's mode hints and
// dynamic prompt suggestions) counts as empty, not a user draft.
func agyIdleInputColor(ansiScreen string) error {
	cleanANSI := strings.NewReplacer("\x00", "", "\r", "").Replace(ansiScreen)
	plain := panelSGR.ReplaceAllString(cleanANSI, "")
	rawLines := strings.Split(strings.TrimRight(cleanANSI, "\n"), "\n")
	plainLines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if len(rawLines) != len(plainLines) {
		return agyIdleInput(plain)
	}
	top, bot, ok := agyBoxBounds(plainLines)
	if !ok {
		return agyNotBoxedErr(plainLines)
	}
	// If all non-whitespace characters after ">" inside the box are styled
	// grey/dim (SGR 2, 90, or 38;5;8), it is a placeholder/suggestion.
	allGrey := true
	var parts []string
	for i := top + 1; i < bot; i++ {
		t := strings.TrimSpace(plainLines[i])
		if i == top+1 {
			t = strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(plainLines[i], " \t"), ">"))
		}
		if t != "" {
			parts = append(parts, t)
			if !agyLineAllGrey(rawLines[i], i == top+1) {
				allGrey = false
			}
		}
	}
	if len(parts) == 0 || allGrey {
		return nil
	}
	joined := strings.Join(parts, " ")
	if agyPlaceholder.MatchString(joined) {
		return nil
	}
	return errDraft
}

func agyNotBoxedErr(lines []string) error {
	// Inspect only the bottom 12 non-blank lines (the active prompt/choice
	// footer region) so stale scrollback or command output above a running
	// status bar cannot trigger a false "waiting for you" popup.
	var tail []string
	for i := len(lines) - 1; i >= 0 && len(tail) < 12; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		tail = append(tail, lines[i])
	}
	foot := strings.Join(tail, "\n")
	for _, w := range agyAsking {
		if strings.Contains(foot, w) {
			return errAgyAsks
		}
	}
	if agyChoiceCursorRe.MatchString(foot) || agyQuestionHeaderRe.MatchString(foot) {
		return errAgyAsks
	}
	return errScreen
}

// agyScreenAsks reports whether an Antigravity CLI screen is currently showing
// a permission confirmation, question, or trust dialog in place of its input box.
func agyScreenAsks(screen string) bool {
	plain := panelSGR.ReplaceAllString(strings.NewReplacer("\x00", "", "\r", "").Replace(screen), "")
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if _, _, ok := agyBoxBounds(lines); ok {
		return false
	}
	return errors.Is(agyNotBoxedErr(lines), errAgyAsks)
}

// agyLineAllGrey reports whether every visible non-whitespace character in
// line (after the leading ">" when firstLine is true) is styled dim or grey
// (SGR 2, 90, or 38;5;8 — how agy renders placeholders and suggestions).
func agyLineAllGrey(line string, firstLine bool) bool {
	grey := false
	pastPrompt := !firstLine
	for len(line) > 0 {
		if loc := sgr.FindStringIndex(line); loc != nil && loc[0] == 0 {
			params := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line[:loc[1]], "\x1b["), "m"), ";")
			for i := 0; i < len(params); i++ {
				switch params[i] {
				case "", "0":
					grey = false
				case "2", "90":
					grey = true
				case "22", "39":
					grey = false
				case "38":
					if i+2 < len(params) && params[i+1] == "5" {
						grey = params[i+2] == "8" || params[i+2] == "240" || params[i+2] == "241" || params[i+2] == "242" || params[i+2] == "243" || params[i+2] == "244" || params[i+2] == "245"
						i += 2
					} else if i+4 < len(params) && params[i+1] == "2" {
						i += 4
						grey = false
					}
				case "30", "31", "32", "33", "34", "35", "36", "37", "91", "92", "93", "94", "95", "96", "97":
					grey = false
				}
			}
			line = line[loc[1]:]
			continue
		}
		r := rune(line[0])
		size := 1
		if r >= 0x80 {
			for _, ch := range line {
				r = ch
				size = len(string(ch))
				break
			}
		}
		line = line[size:]
		if !pastPrompt {
			if unicode.IsSpace(r) {
				continue
			}
			if r == '>' {
				pastPrompt = true
				continue
			}
			return false
		}
		if unicode.IsSpace(r) {
			continue
		}
		if !grey {
			return false
		}
	}
	return pastPrompt
}

// agyBoxBounds finds the top and bottom horizontal rules enclosing agy's
// input box near the bottom of the screen. When background tasks are running,
// agy draws an extra horizontal rule below the task strip, so we walk up
// across candidate rule pairs until finding one whose first inner line begins
// with ">".
func agyBoxBounds(lines []string) (top, bot int, ok bool) {
	// Collect up to 6 horizontal rules within the last 24 non-blank lines.
	var rules []int
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < 24; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		seen++
		if agyRule.MatchString(lines[i]) {
			rules = append(rules, i)
			if len(rules) >= 6 {
				break
			}
		}
	}
	// Check each bottom rule candidate against the nearest rule above it
	// (up to 40 lines taller, to accommodate multi-line pasted messages).
	for k, bot := range rules {
		top := -1
		if k+1 < len(rules) {
			top = rules[k+1]
		} else {
			// Fallback search above the 24 non-blank window for tall multi-line input.
			for i := bot - 2; i >= 0 && bot-i <= 40; i-- {
				if agyRule.MatchString(lines[i]) {
					top = i
					break
				}
			}
		}
		if top < 0 || bot-top < 2 || bot-top > 40 {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(lines[top+1], " \t"), ">") {
			return top, bot, true
		}
	}
	return 0, 0, false
}

// agyBox returns the lines of agy's input box (from its top rule through its
// bottom rule, trailing spaces trimmed), matching agyBoxPipe on the host.
func agyBox(raw []byte) (box []string, ok bool) {
	s := strings.NewReplacer("\x00", "", "\r", "").Replace(string(raw))
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	top, bot, ok := agyBoxBounds(lines)
	if !ok {
		return nil, false
	}
	return lines[top : bot+1], true
}

func agyBoxDigest(raw []byte) (string, bool) {
	box, ok := agyBox(raw)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(strings.Join(box, "\n") + "\n"))
	return hex.EncodeToString(sum[:]), true
}

// agyModeOnScreen reads agy's active execution mode from the status bar below
// its input box: "auto", "plan", "acceptEdits", or "default" when the input
// box is present with no mode prefix; "" when the input box is absent.
var agyModeRe = regexp.MustCompile(`(?:^|\s{2,})(auto|plan|accept-edits)\s+·`)

func agyModeOnScreen(screen string) string {
	plain := panelSGR.ReplaceAllString(strings.NewReplacer("\x00", "", "\r", "").Replace(screen), "")
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	_, bot, ok := agyBoxBounds(lines)
	if !ok {
		return ""
	}
	for i := bot + 1; i < len(lines); i++ {
		if m := agyModeRe.FindStringSubmatch(lines[i]); m != nil {
			switch m[1] {
			case "auto":
				return "auto"
			case "plan":
				return "plan"
			case "accept-edits":
				return "acceptEdits"
			}
		}
	}
	return "default"
}

// agyInputText returns the trimmed text inside agy's input box (without its
// leading ">"), or ok=false when the input box is not visible.
func agyInputText(screen string) (string, bool) {
	plain := panelSGR.ReplaceAllString(screen, "")
	box, ok := agyBox([]byte(plain))
	if !ok || len(box) < 3 {
		return "", false
	}
	var parts []string
	for i := 1; i < len(box)-1; i++ {
		t := strings.TrimSpace(box[i])
		if i == 1 {
			t = strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(box[i], " \t"), ">"))
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " "), true
}

// agyInInput reports whether a sent message is still sitting in agy's input
// box (for sent-missing diagnosis).
func agyInInput(screen, text string) (waiting, known bool) {
	in, ok := agyInputText(screen)
	if !ok {
		return false, false
	}
	want := strings.Join(strings.Fields(text), " ")
	if want == "" {
		return false, true
	}
	if in == "" || agyPlaceholder.MatchString(in) {
		return false, true
	}
	got := strings.Join(strings.Fields(in), " ")
	head := want
	if len(head) > 24 {
		head = head[:24]
	}
	return strings.Contains(got, head), true
}

// agyTypedWhole reports whether agy's input box currently begins with the
// message that was sent (safe to re-press Enter).
func agyTypedWhole(screen, text string) bool {
	in, ok := agyInputText(screen)
	if !ok {
		return false
	}
	want := strings.Join(strings.Fields(text), " ")
	got := strings.Join(strings.Fields(in), " ")
	if want == "" || got == "" || agyPlaceholder.MatchString(got) {
		return false
	}
	head := want
	if len(head) > 32 {
		head = head[:32]
	}
	return strings.HasPrefix(got, head)
}

// agyBoxPipe: agyBox on the host, printing the exact same lines whose SHA-256
// equals agyBoxDigest (empty when the input box is absent, e.g. while agy
// displays a permission prompt, question, or trust dialog). Walks up across
// rule pairs so a background task strip below the input box is skipped cleanly.
// UTF-8 bytes of "─" (U+2500) are \342\224\200 (12 repeats = 36 bytes).
const agyBoxPipe = `tr -d '\000\r' | LC_ALL=C sed 's/[ 	]*$//' | LC_ALL=C awk '
function is_rule(s,  t) {
  t = s
  gsub(/^[ 	]+|[ 	]+$/, "", t)
  if (length(t) < 36) return 0
  gsub(/\342\224\200/, "", t)
  return t == ""
}
{ a[NR] = $0 }
END {
  s = 0; nr = 0
  for (i = NR; i >= 1 && s < 24; i--) {
    t = a[i]; gsub(/^[ 	]+/, "", t)
    if (t == "") continue
    s++
    if (is_rule(a[i])) {
      nr++; r[nr] = i
      if (nr >= 6) break
    }
  }
  for (k = 1; k <= nr; k++) {
    bot = r[k]; top = 0
    if (k + 1 <= nr) {
      top = r[k + 1]
    } else {
      for (i = bot - 2; i >= 1 && bot - i <= 40; i--) {
        if (is_rule(a[i])) { top = i; break }
      }
    }
    if (!top || bot - top < 2 || bot - top > 40) continue
    t = a[top + 1]; sub(/^[ 	]+/, "", t)
    if (substr(t, 1, 1) == ">") {
      for (i = top; i <= bot; i++) print a[i]
      exit
    }
  }
}'`

// agyQueuedBulletRe matches queued user message lines agy renders directly
// above its input box while working, e.g.:
//   ▸ follow-up prompt text (alt+m to send immediately)
var (
	agyQueuedBulletRe = regexp.MustCompile(`^[ \t]*[▸►▶][ \t]+(.+?)\s*$`)
	agyQueuedHintRe   = regexp.MustCompile(`\s*\(alt\+m to send immediately\)\s*$`)
)

// agyQueuedOnScreen extracts queued user messages visible on an Antigravity CLI
// pane screen so Chat Mode can render the same Queue UI as Claude Code.
func agyQueuedOnScreen(screen string) []convo.Queued {
	plain := panelSGR.ReplaceAllString(strings.NewReplacer("\x00", "", "\r", "").Replace(screen), "")
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	top, _, boxed := agyBoxBounds(lines)
	hasFooterHint := false
	footStart := max(0, len(lines)-10)
	if boxed && top+1 < len(lines) {
		footStart = top + 1
	}
	for i := footStart; i < len(lines); i++ {
		if strings.Contains(lines[i], "Press up to edit queued messages") || strings.Contains(lines[i], "alt+m to send immediately") {
			hasFooterHint = true
			break
		}
	}
	// Scan up to 16 lines immediately above the input box (or the bottom 20
	// lines when the box is not yet drawn).
	end := len(lines)
	if boxed {
		end = top
	}
	start := max(0, end-16)
	var out []convo.Queued
	for i := start; i < end; i++ {
		raw := lines[i]
		m := agyQueuedBulletRe.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		body := strings.TrimSpace(m[1])
		// Exclude collapsed attachment lines ("▸ 📎 1 image(s) (277 KB) (ctrl+o to expand)").
		if strings.Contains(body, "(ctrl+o to expand)") || strings.HasPrefix(body, "📎") {
			continue
		}
		hasInlineHint := agyQueuedHintRe.MatchString(body)
		if !hasInlineHint && !hasFooterHint {
			continue
		}
		text := strings.TrimSpace(agyQueuedHintRe.ReplaceAllString(body, ""))
		if text == "" {
			continue
		}
		out = append(out, convo.Queued{Text: text})
		if len(out) >= 16 {
			break
		}
	}
	return out
}

// agyWaits checks whether an Antigravity CLI pane is currently waiting on a
// permission prompt, question, or trust dialog — and in the same ~1ms read
// captures any queued messages visible above agy's input box. Cached on st
// for convoTitleTTL (2s) and invalidated immediately on any press/reply.
// In herdr mode, reads directly over herdr's Unix socket (~1ms, no shell spawn).
func (s *Server) agyWaits(ctx context.Context, sess *term.Session, st *convoState) bool {
	b := st.bind
	who := strings.Join([]string{b.Pane, strconv.FormatInt(b.PID, 10), strconv.FormatInt(b.Start, 10), b.Session}, "\x00")
	pressed, _ := s.agyPressedMap.Load(s.convoReplyKey(sess, b.Pane))
	at, _ := pressed.(time.Time)
	if waits, ok := st.agyWaitKept(who, at, time.Now()); ok {
		return waits
	}
	began := time.Now()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var screen string
	var err error
	if sess.Persistence == "herdr" {
		socket := s.herdrSocks.get(sess.ID)
		if socket != "" {
			var c *herdr.Client
			c, err = herdr.DialWait(rctx, sess.Client(), socket, 2*time.Second)
			if err == nil {
				screen, err = c.ReadPaneSource(rctx, b.Pane, "recent", 60)
				c.Close()
				if err != nil {
					if c2, derr := herdr.DialWait(rctx, sess.Client(), socket, 2*time.Second); derr == nil {
						screen, err = c2.ReadPane(rctx, b.Pane, 0)
						c2.Close()
					}
				}
			}
		} else {
			err = errors.New("no herdr socket")
		}
	} else {
		var res sshx.ExecResult
		res, err = sshx.Exec(rctx, sess.Client(), agyScreenCommand(sess.Persistence, b.Pane, ""), 256<<10, sshx.ExecOptions{})
		if err == nil {
			screen = string(res.Out)
		}
	}
	waits := err == nil && agyScreenAsks(screen)
	var queued []convo.Queued
	if err == nil && !waits {
		queued = agyQueuedOnScreen(screen)
	}
	st.agyWaitAt, st.agyWaitFor, st.agyWait, st.agyQueued, st.agyWaitFail, st.agyWaitFailAt = began, who, waits, queued, err != nil, time.Now()
	return waits
}

// agyPressed marks keys/text/replies sent to an agy pane so the next poll
// re-checks its screen immediately instead of serving a cached wait result.
func (s *Server) agyPressed(sess *term.Session, pane string) {
	s.agyPressedMap.Store(s.convoReplyKey(sess, pane), time.Now())
}

func (st *convoState) agyWaitKept(who string, pressed, now time.Time) (waits, ok bool) {
	fresh := now.Sub(st.agyWaitAt) < convoTitleTTL
	if st.agyWaitFail {
		fresh = now.Sub(st.agyWaitFailAt) < convoTitleBackoff
	}
	if st.agyWaitFor != who || !fresh || !st.agyWaitAt.After(pressed) {
		return false, false
	}
	return st.agyWait, true
}

// isAgyHerdrPane reports whether a herdr pane is known to run Antigravity CLI,
// checking both herdr's reported agent name and Corral's own bound conversation.
func (s *Server) isAgyHerdrPane(sessID, pane string) bool {
	if hp, ok := s.herdrSocks.paneOf(sessID, pane); ok && isAgyAgent(hp.Agent) {
		return true
	}
	v, ok := s.agyPanes.Load(sessID + "\x00" + pane)
	return ok && v.(bool)
}

func (s *Server) noteAgyPane(sessID, pane string, isAgy bool) {
	if pane == "" {
		return
	}
	key := sessID + "\x00" + pane
	if isAgy {
		s.agyPanes.Store(key, true)
	} else {
		s.agyPanes.Delete(key)
	}
}
