package web

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// A Codex conversation copied up to the end of one of its turns (the
// person's "fork from here"): Codex has no way to fork at a point, so its
// copy ("codex fork <id>", all of it) is rewound there in its own terminal,
// as the person would — Esc Esc (its transcript browsed, 0.167: "Browsing
// transcript · ←→/hl prompts · ↵ rewind · esc back", the last of the
// person's messages chosen), ← to the one before as many times as there are
// messages after the one to go back to, Enter ("Conversation reverted to
// this point", that message put back in its input). Which message is chosen
// is shown only by its colour: what came of it is told by its input after,
// holding that message's words, or it is said not to be known.

// rewindPane: the copy's pane, its keys pressed (herdr's names: esc, left,
// enter) and its screen read.
type rewindPane interface {
	press(ctx context.Context, key string) error
	screen(ctx context.Context) (string, error)
}

// tmuxRewindPane: a tmux session of its own, the copy in its one pane —
// that pane by its id, and the process tmux started in it (its pid), as
// first found: each key pressed only while the pane is still that one,
// that process alive in it (review fork2-1).
type tmuxRewindPane struct {
	sess    *term.Session
	pane    string // (%N)
	panePID string
}

// tmuxRewindPaneOf: the session's pane and its process now.
func tmuxRewindPaneOf(ctx context.Context, sess *term.Session, name string) (tmuxRewindPane, bool) {
	out, err := sshx.Output(ctx, sess.Client(), "tmux display-message -p -t "+sshx.QuoteShell("="+name+":")+" '#{pane_id} #{pane_pid} #{pane_dead}'", 1<<10)
	f := strings.Fields(string(out))
	if err != nil || len(f) != 3 || !tmuxPane.MatchString(f[0]) || !isDigits(f[1]) || f[2] != "0" {
		return tmuxRewindPane{}, false
	}
	return tmuxRewindPane{sess: sess, pane: f[0], panePID: f[1]}, true
}

// tmuxRewindCheck: the pane is still the one found, its process alive in it.
func (p tmuxRewindPane) check() string {
	return `set -- $(tmux display-message -p -t ` + sshx.QuoteShell(p.pane) + ` '#{pane_pid} #{pane_dead}' 2>/dev/null); [ "$1" = ` + sshx.QuoteShell(p.panePID) + ` ] && [ "$2" = 0 ] || exit 3; `
}

func (p tmuxRewindPane) press(ctx context.Context, key string) error {
	k := replyPress("tmux", []string{key})[0]
	if key == "ctrl+c" {
		k = "C-c"
	}
	_, err := sshx.Output(ctx, p.sess.Client(), p.check()+"tmux send-keys -t "+sshx.QuoteShell(p.pane)+" "+sshx.QuoteShell(k), 1<<10)
	return err
}

func (p tmuxRewindPane) screen(ctx context.Context) (string, error) {
	out, err := sshx.Output(ctx, p.sess.Client(), p.check()+"tmux -u capture-pane -p -t "+sshx.QuoteShell(p.pane), 256<<10)
	return string(out), err
}

// herdrRewindPane: a herdr pane, by herdr's own requests — each on a
// connection of its own (one kept across them was found closed under it,
// 11-16: "EOF"), waiting a moment for the side operations' slot.
type herdrRewindPane struct {
	sess   *term.Session
	socket string
	pane   string
}

func (p herdrRewindPane) with(ctx context.Context, f func(c *herdr.Client) error) error {
	c, err := herdr.DialWait(ctx, p.sess.Client(), p.socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	return f(c)
}

func (p herdrRewindPane) press(ctx context.Context, key string) error {
	return p.with(ctx, func(c *herdr.Client) error { return c.SendKeys(ctx, p.pane, []string{key}) })
}

func (p herdrRewindPane) screen(ctx context.Context) (s string, err error) {
	err = p.with(ctx, func(c *herdr.Client) error {
		s, err = c.ReadPaneSource(ctx, p.pane, "visible", 0)
		return err
	})
	return s, err
}

// codexRewindMax: the most of the person's messages gone back over.
const codexRewindMax = 200

// codexRewindStep: how long between a key and the next look.
var codexRewindStep = 300 * time.Millisecond

// codexRewind rewinds the copy in pane to before the person's message that
// is back-th from the last (1: the last), its words at (their start): true
// once its input is seen holding them. Nothing pressed where its input is not
// seen ready; nothing more where its transcript is not seen browsed. (Not
// done: which step, logged.)
func codexRewind(ctx context.Context, p rewindPane, back int, at string, clear bool) bool {
	why := codexRewindDo(ctx, p, back, at, clear)
	if why != "" {
		slog.Warn("codex fork: not rewound", "step", why)
	}
	return why == ""
}

func codexRewindDo(ctx context.Context, p rewindPane, back int, at string, clear bool) (why string) {
	if back < 1 || back > codexRewindMax || strings.TrimSpace(at) == "" {
		return "asked"
	}
	// (Its input in front and empty, it at rest, as it is once started: not a
	// dialog, nor at work, nor still drawing — review fork2-7.)
	if !rewindWait(ctx, p, 15*time.Second, func(s string) bool {
		return codexIdleInput(s) == nil && !strings.Contains(s, "Browsing transcript")
	}) {
		return "not ready"
	}
	if err := p.press(ctx, "esc"); err != nil {
		return "esc: " + err.Error()
	}
	sleepCtx(ctx, codexRewindStep)
	if err := p.press(ctx, "esc"); err != nil {
		return "esc: " + err.Error()
	}
	if !rewindWait(ctx, p, 4*time.Second, func(s string) bool { return strings.Contains(s, "Browsing transcript") }) {
		return "not browsed"
	}
	for i := 1; i < back; i++ {
		if err := p.press(ctx, "left"); err != nil {
			return "left: " + err.Error()
		}
		sleepCtx(ctx, codexRewindStep/2)
	}
	sleepCtx(ctx, codexRewindStep)
	if err := p.press(ctx, "enter"); err != nil {
		return "enter: " + err.Error()
	}
	// (Its words back in its input — the first line of them, where it may be
	// folded or cut — the transcript no longer browsed.)
	want := []rune(squeeze(strings.SplitN(strings.TrimSpace(at), "\n", 2)[0]))
	want = want[:min(len(want), 40)]
	// (Its words back in its input, it at rest — words there, not its
	// placeholder, which a message of a few of its letters would match.)
	if !rewindWait(ctx, p, 5*time.Second, func(s string) bool {
		area, _, _, ok := codexInputArea(s)
		box := strings.TrimPrefix(squeeze(area), "›")
		return ok && len(want) > 0 && !strings.Contains(s, "Browsing transcript") && errors.Is(codexIdleInput(s), errDraft) && !codexEmptyInput(area) && strings.HasPrefix(box, string(want))
	}) {
		return "not seen in its input"
	}
	if !clear {
		return "" // (left in its input: a picture in it, or too long for the page's)
	}
	// (Its input emptied — Ctrl+C on words there clears them, nothing else —
	// for the page to send to: that message is put in the page's own input,
	// to change and send, rather than left where a message from the page
	// would be refused as words not its own.)
	if err := p.press(ctx, "ctrl+c"); err != nil {
		return "ctrl+c: " + err.Error()
	}
	if !rewindWait(ctx, p, 3*time.Second, func(s string) bool { return codexIdleInput(s) == nil }) {
		return "not emptied"
	}
	return ""
}

// rewindWait reads the screen now and then till ok says it shows what is
// waited for, or the time is up.
func rewindWait(ctx context.Context, p rewindPane, within time.Duration, ok func(string) bool) bool {
	until := time.Now().Add(within)
	for {
		if s, err := p.screen(ctx); err == nil && ok(s) {
			return true
		}
		if time.Now().After(until) || ctx.Err() != nil {
			return false
		}
		sleepCtx(ctx, codexRewindStep)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
