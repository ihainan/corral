package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"time"

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
// more than that. No key goes unless pressed; this does not resize the pane or
// bring it in front, as the terminal page does.

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

func (s *Server) convoPanelScreen(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), convoColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: release})
	if err != nil {
		// (Given up on by the page, a key having come: nothing to say of it, and
		// nothing to note — it is no failure to read.)
		if ctx.Err() != nil {
			*outcome, *reason = "unknown", "cancelled"
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
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "digest": digest})
}

// panelDigest names a screen as read with its colours, without them: a cursor
// blinking in a panel's search box changes only its colours, and would never
// let a key that goes with the screen through. Read off that one read, so that
// it is of what is shown. The gate computes the same on the host: panelDigestPipe
// (cross-checked in a test).
func panelDigest(raw []byte) string {
	b := bytes.ReplaceAll(raw, []byte{0}, nil)
	b = steadyScreen(panelSGR.ReplaceAll(b, nil))
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(append([]byte(nil), b...), '\n')
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var panelSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// panelDigestPipe: panelDigest as a shell pipeline, of what comes in.
const panelDigestPipe = `tr -d '\000' | LC_ALL=C sed "s/$(printf '\033')\[[0-9;:]*m//g" | ` + steadyPipe + ` | LC_ALL=C awk 1 | sha256sum`

// A tool's row under way blinks: its "●" (macOS's "⏺") goes to a blank and back
// a few times a second, on a screen where nothing else moves. Counted, the
// digest went between two values, and half of all answers to the prompt under
// it were refused as "the screen changed" (11-06). So a line's first mark is
// left out of what names a screen; both sides, steadyScreen and steadyPipe.
var blinkingMark = regexp.MustCompile(`(?m)^([ \t]*)(?:●|⏺)`)

func steadyScreen(raw []byte) []byte { return blinkingMark.ReplaceAll(raw, []byte("${1} ")) }

const steadyPipe = `LC_ALL=C sed -e 's/^\([ 	]*\)●/\1 /' -e 's/^\([ 	]*\)⏺/\1 /'`

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
