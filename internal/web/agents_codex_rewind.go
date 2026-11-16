package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
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
	typeText(ctx context.Context, text string) error // (as typed: no Enter)
	screen(ctx context.Context) (string, error)
	title(ctx context.Context) (string, error) // (the terminal's title, as the agent set it)
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

// tmuxMadePane: the pane a new tmux session was made with, as
// agentTmuxCmd's output says it ("pane %N PID", as tmux said it on making
// it — not whichever pane is active there later: review fork3-4).
func tmuxMadePane(sess *term.Session, out string) (tmuxRewindPane, bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "pane" && tmuxPane.MatchString(f[1]) && isDigits(f[2]) {
			return tmuxRewindPane{sess: sess, pane: f[1], panePID: f[2]}, true
		}
	}
	return tmuxRewindPane{}, false
}

// tmuxRewindCheck: the pane is still the one found, its process alive in it.
// (Run by sh, whatever the account's shell: zsh does not split words — review fork4b-4.)
func (p tmuxRewindPane) check() string {
	return `[ "$(tmux display-message -p -t ` + sshx.QuoteShell(p.pane) + ` '#{pane_pid} #{pane_dead}' 2>/dev/null)" = ` + sshx.QuoteShell(p.panePID+" 0") + ` ] || exit 3; `
}

func (p tmuxRewindPane) press(ctx context.Context, key string) error {
	k := replyPress("tmux", []string{key})[0]
	if key == "ctrl+c" {
		k = "C-c"
	}
	_, err := sshx.Output(ctx, p.sess.Client(), p.check()+"tmux send-keys -t "+sshx.QuoteShell(p.pane)+" "+sshx.QuoteShell(k), 1<<10)
	return err
}

func (p tmuxRewindPane) typeText(ctx context.Context, text string) error {
	_, err := sshx.Output(ctx, p.sess.Client(), p.check()+"tmux send-keys -l -t "+sshx.QuoteShell(p.pane)+" -- "+sshx.QuoteShell(text), 1<<10)
	return err
}

func (p tmuxRewindPane) title(ctx context.Context) (string, error) {
	out, err := sshx.Output(ctx, p.sess.Client(), p.check()+"tmux -u display-message -p -t "+sshx.QuoteShell(p.pane)+" '#{pane_title}'", 4<<10)
	return strings.TrimRight(string(out), "\n"), err
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
	// (The Codex found in it once started, by its pid and start: each key
	// pressed only while that very process is in front there — another in
	// the pane since is not keyed to: review fork5-1.)
	pid, start int64
}

// herdrRewindPaneOf: the pane's one Codex, as a look begun now finds it —
// and that one started as the copy of src ("codex fork <src>", its command
// line says), not another put there since (review fork6-1).
func (s *Server) herdrRewindPaneOf(ctx context.Context, sess *term.Session, socket, pane, src string) (herdrRewindPane, bool) {
	var c codexPane
	ok := false
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		if panes, err := s.codexPanes.run(ctx, sess, socket, pane); err == nil {
			if c, ok = panes[pane]; ok && !c.Several && c.PID > 0 && c.Start > 0 {
				break
			}
			ok = false
		}
		sleepCtx(ctx, 500*time.Millisecond)
	}
	if !ok || !codexForkOf(ctx, sess, c.PID, c.Start, src) {
		return herdrRewindPane{}, false
	}
	return herdrRewindPane{sess: sess, socket: socket, pane: pane, pid: c.PID, start: c.Start}, true
}

// codexForkOf: process pid (of that start; 0: any) was started "codex
// fork <src>".
func codexForkOf(ctx context.Context, sess *term.Session, pid, start int64, src string) bool {
	cmd := `pid=` + strconv.FormatInt(pid, 10) + ` start=` + strconv.FormatInt(start, 10) + `
stat=$(tr '\n' ' ' <"/proc/$pid/stat" 2>/dev/null) || exit 3
rest=${stat##*) }
[ "$start" = 0 ] || [ "$(printf '%s' "$rest" | cut -d' ' -f20)" = "$start" ] || exit 3
[ "$(head -c 16 "/proc/$pid/comm" 2>/dev/null)" = codex ] || exit 3
tr '\0' '\001' <"/proc/$pid/cmdline" 2>/dev/null | awk -v want=fork '` + codexArgsAwk + `'`
	out, err := sshx.Output(ctx, sess.Client(), "sh -c "+sshx.QuoteShell(cmd), 128)
	return err == nil && src != "" && strings.EqualFold(strings.TrimSpace(string(out)), src)
}

// codexForkOfPid: codexForkOf of the process pid, whatever its start.
func codexForkOfPid(ctx context.Context, sess *term.Session, pid int64, src string) bool {
	return pid > 0 && codexForkOf(ctx, sess, pid, 0, src)
}

// sendSh: a command that presses (send-keys) or types (send-text) arg in
// the pane — by herdr's own command, in the same command as the check that
// that very process is alive (its start), in front on its terminal, in that
// pane: nothing between the check and the key (review fork6-2).
func (p herdrRewindPane) sendSh(how, arg string) string {
	q := sshx.QuoteShell
	return `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
pid=` + strconv.FormatInt(p.pid, 10) + ` start=` + strconv.FormatInt(p.start, 10) + ` pane=` + q(p.pane) + `
stat=$(tr '\n' ' ' <"/proc/$pid/stat" 2>/dev/null) || exit 3
rest=${stat##*) }
[ "$(printf '%s' "$rest" | cut -d' ' -f20)" = "$start" ] || exit 3
pgrp=$(printf '%s' "$rest" | cut -d' ' -f3); tpgid=$(printf '%s' "$rest" | cut -d' ' -f6)
[ -n "$pgrp" ] && [ "$pgrp" = "$tpgid" ] || exit 3
tr '\0\n' '\n\001' <"/proc/$pid/environ" 2>/dev/null | grep -qxF "HERDR_PANE_ID=$pane" || exit 3
HERDR_SOCKET_PATH=` + q(p.socket) + ` herdr pane ` + how + ` "$pane" ` + q(arg) + ` >/dev/null 2>&1 || exit 6
echo ok`
}

func (p herdrRewindPane) send(ctx context.Context, how, arg string) error {
	out, err := sshx.Output(ctx, p.sess.Client(), "sh -c "+sshx.QuoteShell(p.sendSh(how, arg)), 64)
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		return errors.New("not sent: not the Codex it was, or herdr refused it")
	}
	return nil
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
	return p.send(ctx, "send-keys", key)
}

func (p herdrRewindPane) typeText(ctx context.Context, text string) error {
	return p.send(ctx, "send-text", text)
}

func (p herdrRewindPane) title(ctx context.Context) (t string, err error) {
	err = p.with(ctx, func(c *herdr.Client) error {
		snap, err := c.Snapshot(ctx)
		if err != nil {
			return err
		}
		for _, pn := range snap.Panes {
			if pn.ID == p.pane {
				t = pn.Title
				return nil
			}
		}
		return errors.New("no such pane")
	})
	return t, err
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
	// (Each key only on the screen it is for, read just before — another
	// connection to the terminal may have done something meanwhile: review
	// fork4b-1.)
	if !rewindWait(ctx, p, 3*time.Second, func(s string) bool { return strings.Contains(s, "esc again to edit previous message") }) {
		return "not asked for the second esc"
	}
	if err := p.press(ctx, "esc"); err != nil {
		return "esc: " + err.Error()
	}
	browsing := func(s string) bool { return strings.Contains(s, "Browsing transcript") }
	if !rewindWait(ctx, p, 4*time.Second, browsing) {
		return "not browsed"
	}
	for i := 1; i < back; i++ {
		if !rewindWait(ctx, p, 2*time.Second, browsing) {
			return "no longer browsed"
		}
		if err := p.press(ctx, "left"); err != nil {
			return "left: " + err.Error()
		}
		sleepCtx(ctx, codexRewindStep/2)
	}
	sleepCtx(ctx, codexRewindStep)
	if !rewindWait(ctx, p, 2*time.Second, browsing) {
		return "no longer browsed"
	}
	if err := p.press(ctx, "enter"); err != nil {
		return "enter: " + err.Error()
	}
	// (Its words back in its input — the first line of them, where it may be
	// folded or cut — the transcript no longer browsed.)
	want := []rune(squeeze(strings.SplitN(strings.TrimSpace(at), "\n", 2)[0]))
	want = want[:min(len(want), 40)]
	// (Its words back in its input, it at rest — words there, not its
	// placeholder, which a message of a few of its letters would match.)
	if !rewindWait(ctx, p, 5*time.Second, func(s string) bool { return codexInputHolds(s, want) }) {
		return "not seen in its input"
	}
	if !clear {
		return "" // (left in its input: a picture in it, or too long for the page's)
	}
	// (Its input emptied — Ctrl+C on words there clears them, nothing else —
	// for the page to send to: that message is put in the page's own input,
	// to change and send, rather than left where a message from the page
	// would be refused as words not its own.)
	if !rewindWait(ctx, p, time.Second, func(s string) bool { return codexInputHolds(s, want) }) {
		return "no longer in its input"
	}
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

// A copy started from the page is named — "/rename <name>", typed in its own
// terminal as the person would — a name of its own: then its pane's title
// shows that name while it is at that conversation (and not once it has
// gone to another, its original say), which tells it, as a named Codex is
// told in herdr and tmux alike (codexPressSure). Till named it shows its
// original's name, copied with it, and no word on which it is at.

// codexForkName: the name given a copy, from the fork's label.
func codexForkName(label string) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		label = "fork"
	}
	return label + " " + hex.EncodeToString(b)
}

// codexNameCopy names the copy in p (its folder cwd) name: true once its pane's
// title is seen showing it. Typed only with Codex at rest, its input empty.
func codexNameCopy(ctx context.Context, p rewindPane, name, cwd string) bool {
	why := codexNameDo(ctx, p, name, cwd)
	if why != "" {
		slog.Warn("codex fork: not named", "step", why)
	}
	return why == ""
}

func codexNameDo(ctx context.Context, p rewindPane, name, cwd string) string {
	var idle string
	if !rewindWait(ctx, p, 15*time.Second, func(s string) bool {
		if codexIdleInput(s) != nil || strings.Contains(s, "Browsing transcript") {
			return false
		}
		idle = s
		return true
	}) {
		return "not ready"
	}
	// (Its status line as it is at rest: under what is typed, only that —
	// a line of a draft's is not — review fork6-3.)
	status := codexRestStatus(idle)
	if err := p.typeText(ctx, "/rename "+name); err != nil {
		return "typed: " + err.Error()
	}
	// (What was typed seen whole in its input, nothing else there, before Enter.)
	// (Nothing else in its input — no words under it taken for its footer —
	// and it at rest: review fork4b-5.)
	held := func(s string) bool { return codexHoldsOnly(s, "/rename "+name, status) }
	if !rewindWait(ctx, p, 3*time.Second, held) {
		return "not seen typed"
	}
	if err := p.press(ctx, "enter"); err != nil {
		// (Refused at once — herdr still taking the words before it, once in
		// some runs —: pressed again only where they are seen still there,
		// alone, Codex at rest: not twice on a screen it went on from.)
		sleepCtx(ctx, codexRewindStep)
		if !rewindWait(ctx, p, 2*time.Second, held) {
			return "enter: " + err.Error()
		}
		if err := p.press(ctx, "enter"); err != nil {
			return "enter: " + err.Error()
		}
	}
	until := time.Now().Add(5 * time.Second)
	for {
		if t, err := p.title(ctx); err == nil {
			if thread, bare := codexTitleParts("codex", t, cwd); !bare && thread == name {
				return ""
			}
		}
		if time.Now().After(until) || ctx.Err() != nil {
			return "title not seen"
		}
		sleepCtx(ctx, codexRewindStep)
	}
}

// codexForkAfter: a Codex copy just started from the page (in p, if found):
// rewound where asked (out "rewound"), then named (out "named") — so that
// its chat can tell it; the folder dir, a name from label.
func (s *Server) codexForkAfter(ctx context.Context, out map[string]any, p rewindPane, found bool, how agentStart, label, dir string) {
	if !found {
		if how.back > 0 {
			out["rewound"] = false
		}
		out["named"] = false
		return
	}
	// (Named first: /rename is no message of the person's — its transcript
	// steps over it — and a rewind may leave words in its input after, a
	// picture's say, that nothing is to be typed over: review fork4b-7.)
	out["named"] = codexNameCopy(ctx, p, codexForkName(label), dir)
	if how.back > 0 {
		out["rewound"] = codexRewind(ctx, p, how.back, how.atText, how.clear)
	}
}

// codexInputHolds: Codex at rest, its input in front holding words that
// begin as want does (letters, spaces squeezed out) — words, not its
// placeholder, which a message of a few of its letters would match; its
// footer gone or not, as words there hide its hints and a default status
// line has none (review fork3-9: codexIdleInput takes no footer for no
// screen of its).
func codexInputHolds(s string, want []rune) bool {
	if len(want) == 0 || strings.Contains(s, "Browsing transcript") || strings.Contains(s, "esc to interrupt") {
		return false
	}
	area, _, _, ok := codexInputArea(s)
	if !ok || codexEmptyInput(area) {
		return false
	}
	return strings.HasPrefix(strings.TrimPrefix(squeeze(area), "›"), string(want))
}

// codexHoldsOnly: Codex at rest, its input holding typed and nothing else —
// under it nothing, or its status line as it was at rest (status: of a
// status line of the person's, whatever it shows — review fork5-2; not a
// line of a draft's taken for one — fork6-3), or a footer known.
func codexHoldsOnly(s, typed, status string) bool {
	if strings.Contains(s, "esc to interrupt") || strings.Contains(s, "Browsing transcript") {
		return false
	}
	area, foot, known, ok := codexInputArea(s)
	if !ok || squeeze(area) != squeeze(typed) {
		return false
	}
	return foot == "" || known || (status != "" && strings.TrimSpace(foot) == status)
}

// codexRestStatus: of a screen of Codex at rest, its input empty, the first
// line of its footer — its status line, where it has one (its hints, "? for
// shortcuts", are gone once words are typed) — "" where none.
func codexRestStatus(s string) string {
	_, foot, _, ok := codexInputArea(s)
	if !ok || foot == "" {
		return ""
	}
	first := strings.TrimSpace(strings.SplitN(foot, "\n", 2)[0])
	if codexEmptyHints.MatchString(first) {
		return ""
	}
	return first
}
