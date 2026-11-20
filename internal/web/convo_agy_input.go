package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"unicode"

	"github.com/ihainan/corral/internal/sshx"
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
var agyAsking = []string{
	"Requesting permission for:",
	"Run this command?",
	"Do you trust the contents of this project?",
	"↑/↓ Navigate",
	"Question ",
}

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
	plain := sgr.ReplaceAllString(cleanANSI, "")
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
	foot := strings.Join(lines[max(0, len(lines)-30):], "\n")
	for _, w := range agyAsking {
		if strings.Contains(foot, w) {
			return errAgyAsks
		}
	}
	return errScreen
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
	plain := sgr.ReplaceAllString(strings.NewReplacer("\x00", "", "\r", "").Replace(screen), "")
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
	plain := sgr.ReplaceAllString(screen, "")
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
