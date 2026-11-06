package web

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// A panel of Claude Code's own (/rewind, /resume, /mcp, /config…) is drawn on
// its screen and tells the log nothing till it is shut, so the conversation
// shows it as the screen does and passes on the keys pressed: only while one
// is still open (its session file says so: the gate "dialog"). A key that
// settles something (Enter, Space, a digit: /config's flips a setting at once)
// goes only onto the screen the person was shown, "dialog:<digest>".

// panelKeyOK: what may be pressed in a panel: moving, confirming, cancelling,
// and the letters its search takes (not words pasted: a list takes them key by key).
var panelLetter = regexp.MustCompile(`^[A-Za-z0-9._/]$`)

func panelKeyOK(k string) bool {
	return replyKeys[k] || k == "shift+tab" || k == "backspace" || panelLetter.MatchString(k)
}

// panelSettles: a key whose effect depends on what is chosen on the screen.
func panelSettles(k string) bool {
	return k == "enter" || k == "space" || (len(k) == 1 && k[0] >= '0' && k[0] <= '9')
}

// panelOK: the one key pressed, and the digest it goes with where it settles something.
func panelOK(keys []string, digest string) bool {
	if len(keys) != 1 || !panelKeyOK(keys[0]) {
		return false
	}
	return !panelSettles(keys[0]) || hexDigest.MatchString(digest)
}

// convoPanelScreenCommand prints a pane's screen with its colours, then a
// line \001D and the digest of the screen as a key's gate reads it (without
// them, see the gate "reply"). Fixed script; the mode, the validated pane id
// and the socket from discovery are its arguments.
func convoPanelScreenCommand(mode, pane, socket string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then
  tmux -u capture-pane -p -e -t "$2" 2>/dev/null 9>&- || exit 8
  printf '\001D '; tmux -u capture-pane -p -t "$2" 2>/dev/null 9>&- | sha256sum 2>/dev/null
else
  HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source visible --format ansi 2>/dev/null 9>&- || exit 8
  printf '\001D '; HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source visible 2>/dev/null 9>&- | sha256sum 2>/dev/null
fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(socket)
}

// panelScreenOf splits what convoPanelScreenCommand printed. A digest not
// read (no sha256sum there) is "": keys that settle something are then refused.
func panelScreenOf(out []byte) (screen, digest string) {
	at := bytes.LastIndex(out, []byte("\x01D "))
	if at < 0 {
		return strings.TrimRight(string(out), " \t\r\n"), ""
	}
	d, _, _ := strings.Cut(string(out[at+3:]), " ")
	if !hexDigest.MatchString(d) {
		d = ""
	}
	text, _, _ := screenOf(out[:at])
	return text, d
}

func (s *Server) convoPanelScreen(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), convoPanelScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: release})
	if err != nil {
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	*outcome = "looked"
	text, digest := panelScreenOf(res.Out)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "digest": digest})
}

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
