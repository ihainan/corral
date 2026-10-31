package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Answering Claude Code from the conversation, in a tmux pane (or a herdr
// one) as in herdr's own answer (see apiHerdrReply): its screen read, then
// keys pressed or a line typed, only while it waits for an answer and its
// screen is still the one answered. Both go as the conversation's other
// presses do (convoPress): to the process it shows, in front in its pane.
// The waiting and the screen are checked on the host right before the keys
// (convoGatedCommand's gate): Claude Code's session file says it waits
// for an answer, and the screen's SHA-256 is the one read here.

// POST /api/sessions/{id}/conversation/screen (the same body as a send,
// with no text) → {"text": …, "digest": …, "cut": …}
func (s *Server) apiConversationScreen(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "screen")
}

// POST /api/sessions/{id}/conversation/reply (as a send, with the digest
// screen gave, and keys or text) → {"ok": true}; 409 not_waiting,
// screen_changed, already_answered, cannot_verify.
func (s *Server) apiConversationReply(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "reply")
}

func (s *Server) convoScreen(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), convoScreenJoinedCommand(sess.Persistence, pane, socket), 512<<10+unwrapMax+64, sshx.ExecOptions{Release: release})
	var shown []byte
	var joined string
	if err == nil {
		var ok bool
		if shown, joined, ok = screenJoined(res.Out); !ok {
			err = errors.New("the screen's read is not framed")
		}
	}
	if err != nil {
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	*outcome = "looked"
	// (Shown with the lines the terminal wrapped joined; its digest that of
	// the screen as the host reads it before an answer's keys.)
	_, digest, _ := screenOf(shown)
	text, _, cut := screenOf([]byte(joined))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "digest": digest, "cut": cut})
}

// screenOf: a screen as read (convoScreenCommand's output) as it is shown
// (without blank space at its end, its last maxScreenBytes), and the digest
// that names it: the SHA-256 of all of it as read, which the host computes
// again right before an answer's keys.
func screenOf(raw []byte) (text, digest string, cut bool) {
	sum := sha256.Sum256(raw)
	text = strings.TrimRight(strings.ReplaceAll(string(raw), "\r\n", "\n"), " \t\n")
	if len(text) > maxScreenBytes {
		at := len(text) - maxScreenBytes
		for at < len(text) && !utf8.RuneStart(text[at]) {
			at++
		}
		text, cut = text[at:], true
	}
	return text, hex.EncodeToString(sum[:]), cut
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// gateRefused: what the gate of convoGatedCommand stopped (exit 9), or a
// text typed and its Enter held back (5): it waits for an answer.
func gateRefused(err error) bool {
	var e *sshx.ExitError
	return errors.As(err, &e) && (e.Status == 9 || e.Status == 5)
}

// replyOK: an answer is a digest and keys (of replyKeys, one to four) or a
// line of text (not both).
func replyOK(digest string, keys []string, text string) bool {
	if !hexDigest.MatchString(digest) {
		return false
	}
	switch {
	case text != "" && len(keys) == 0:
		return utf8.RuneCountInString(text) <= maxReplyText && utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl)
	case text == "" && len(keys) >= 1 && len(keys) <= 4:
		for _, k := range keys {
			if !replyKeys[k] {
				return false
			}
		}
		return true
	}
	return false
}

// replyPress: an answer's keys (herdr's names) as the pane's kind takes them.
func replyPress(mode string, keys []string) []string {
	if mode == "herdr" {
		return keys
	}
	tmux := map[string]string{"enter": "Enter", "esc": "Escape", "tab": "Tab", "space": "Space", "up": "Up", "down": "Down", "left": "Left", "right": "Right"}
	out := make([]string, len(keys))
	for i, k := range keys {
		if t, ok := tmux[k]; ok {
			out[i] = t
		} else {
			out[i] = k // (a digit)
		}
	}
	return out
}

// convoReplyKey: what answers to a pane take turns by — the pane itself,
// whichever of the person's connections to it they come by: a tmux pane's
// id is its server's, of that user on that host; a herdr pane's, its
// server's (its socket on that host).
func (s *Server) convoReplyKey(sess *term.Session, pane string) string {
	if sess.Persistence == "tmux" {
		return fmt.Sprintf("tmux\x00%d\x00%d\x00%s", sess.OwnerID, sess.HostID, pane)
	}
	return fmt.Sprintf("herdr\x00%d\x00%d\x00%s\x00%s", sess.OwnerID, sess.HostID, s.herdrSocks.get(sess.ID), pane)
}
