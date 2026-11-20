package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/ihainan/corral/internal/sshx"
)

// agyScreenCommand prints the live bottom of an agy pane. In herdr,
// --source recent --lines 60 reads the live tail even when the user has
// scrolled up in the pane (whereas --source visible returns the scrolled-up
// viewport without the input box), falling back to --source visible if needed.
func agyScreenCommand(mode, pane, socket string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then tmux -u capture-pane -p -t "$2" 2>/dev/null 9>&-
else HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source recent --lines 60 2>/dev/null 9>&- || HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source visible 2>/dev/null 9>&-; fi`
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
// "> ...") is visible at the foot of the screen and holds no user draft.
// Returns errAgyAsks when a prompt/question replaces the box, errDraft when
// the box holds typed text, or errScreen when the layout is unrecognized.
func agyIdleInput(screen string) error {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	top, bot, ok := agyBoxBounds(lines)
	if !ok {
		foot := strings.Join(lines[max(0, len(lines)-30):], "\n")
		for _, w := range agyAsking {
			if strings.Contains(foot, w) {
				return errAgyAsks
			}
		}
		return errScreen
	}
	first := strings.TrimLeft(lines[top+1], " \t")
	if !strings.HasPrefix(first, ">") {
		return errScreen
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

// agyBoxBounds finds the top and bottom horizontal rules enclosing agy's
// input box near the bottom of the screen.
func agyBoxBounds(lines []string) (top, bot int, ok bool) {
	seen := 0
	bot = -1
	for i := len(lines) - 1; i >= 0 && seen < 8; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		seen++
		if agyRule.MatchString(lines[i]) {
			bot = i
			break
		}
	}
	if bot < 2 {
		return 0, 0, false
	}
	// Search up to 38 lines above the bottom rule for the matching top rule
	// (tall enough for multi-line pasted messages at gate_enter).
	for i := bot - 2; i >= 0 && bot-i <= 40; i-- {
		if agyRule.MatchString(lines[i]) {
			if strings.HasPrefix(strings.TrimLeft(lines[i+1], " \t"), ">") {
				return i, bot, true
			}
			return 0, 0, false
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

// agyBoxPipe: agyBox on the host, printing the exact same lines whose SHA-256
// equals agyBoxDigest (empty when the input box is absent, e.g. while agy
// displays a permission prompt, question, or trust dialog).
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
  s = 0; bot = 0
  for (i = NR; i >= 1 && s < 8; i--) {
    t = a[i]; gsub(/^[ 	]+/, "", t)
    if (t == "") continue
    s++
    if (is_rule(a[i])) { bot = i; break }
  }
  if (bot < 3) exit
  top = 0
  for (i = bot - 2; i >= 1 && bot - i <= 40; i--) {
    if (is_rule(a[i])) {
      t = a[i + 1]; sub(/^[ 	]+/, "", t)
      if (substr(t, 1, 1) == ">") top = i
      break
    }
  }
  if (!top) exit
  for (i = top; i <= bot; i++) print a[i]
}'`
