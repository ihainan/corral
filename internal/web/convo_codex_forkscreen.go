package web

import (
	"bytes"
	"context"
	"path"
	"strconv"
	"strings"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// A copy's screen ("codex fork <id>", its log its daemon's) shows the
// original's words — what came before it was copied — as its own until it
// is written to: its screen then says the original, not which it is at.
// It is taken to show the copy only where that can be told: the copy has
// none of the person's messages of its own yet, and the screen shows none of
// what the original said after the copy was made (past where the copy's
// first line says it goes on from, history_base). Else not told: a Codex
// copied, gone back to its original in its own terminal (/resume), shows
// what the original said since, and keys typed there go to the original.
// (One that has said nothing since the copy was made is not told from the
// copy by its screen — nothing else on the host says which, 0.167.)

// forkPreMax, forkPostMax: how much of the original is read, before and
// after where the copy goes on from.
const (
	forkPreMax  = 256 << 10
	forkPostMax = 512 << 10
)

// forkShowsItsOwn: the copy (p, its log copy, a copy of src) is what its
// screen shows, as far as can be told (see above). False on anything not
// read, or not read whole.
func (s *Server) forkShowsItsOwn(ctx context.Context, sess *term.Session, pane string, p *convoProc, copyLog, src string) bool {
	head, err := sshx.Output(ctx, sess.Client(), "head -c "+strconv.Itoa(maxForkLine)+" "+sshx.QuoteShell(copyLog)+" 2>/dev/null | head -n 1", maxForkLine+1)
	if err != nil {
		return false
	}
	b, ok := codexHistoryBase(append(bytes.TrimRight(head, "\n"), '\n'))
	if !ok {
		return false
	}
	// <home>/sessions/<year>/<month>/<day>/rollout-…: the original's log by its writer, there.
	sessions := path.Dir(path.Dir(path.Dir(path.Dir(copyLog))))
	if path.Base(sessions) != "sessions" {
		return false
	}
	out, err := sshx.Output(ctx, sess.Client(), forkScreenReadCommand(sessions, copyLog, b), forkPreMax+forkPostMax+forkCopyMax+(16<<10))
	if err != nil {
		return false
	}
	r, ok := parseForkScreenRead(out, b, src)
	if !ok {
		return false
	}
	// (Written to — a message of the person's in what it goes on with —: its
	// screen would show its own words, not only the original's.)
	c := convo.NewCodex()
	c.Feed([]byte(r.copy))
	for _, e := range c.Since(0) {
		if e.Role == convo.RoleUser && !e.Gone {
			return false
		}
	}
	said := forkSaidAfter(r.pre, r.post)
	if len(said) == 0 {
		return true // (nothing said since: nothing for its screen to show of it)
	}
	target := pane
	if sess.Persistence == "tmux" {
		target = p.tmuxPane
	}
	scr, err := sshx.Output(ctx, sess.Client(), convoScreenCommand(sess.Persistence, target, s.herdrSocks.get(sess.ID)), 256<<10)
	if err != nil {
		return false
	}
	// (Its conversation only, above its input: the words put back in its
	// input by a rewind are of after where it goes on from, and are no word
	// on which it is at.)
	lines := strings.Split(strings.TrimRight(string(scr), "\n"), "\n")
	if at, _, _, ok := codexInputSpan(lines); ok {
		lines = lines[:at]
	}
	screen := letters(strings.Join(lines, "\n"))
	for _, w := range said {
		if strings.Contains(screen, w) {
			return false // (shows what the original said since: at the original)
		}
	}
	return true
}

// forkCopyMax: the most of the copy's own log read (more: taken as written to).
const forkCopyMax = 4 << 20

// forkRead: what forkScreenReadCommand read: the original's log before and
// after where the copy goes on from, and the copy's own log.
type forkRead struct{ pre, post, copy string }

// parseForkScreenRead: forkScreenReadCommand's output, each part read whole
// (by the sizes the host said), the log gone on from the original's (src's).
func parseForkScreenRead(out []byte, b codexBase, src string) (forkRead, bool) {
	parts := strings.SplitN(string(out), "\x00", 4)
	if len(parts) != 4 {
		return forkRead{}, false
	}
	f := strings.Fields(parts[0])
	if len(f) != 4 || f[0] != "ok" {
		return forkRead{}, false
	}
	size, err1 := strconv.ParseInt(f[2], 10, 64)
	csize, err2 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil || !strings.EqualFold(convoSessionOf(f[1]), src) {
		return forkRead{}, false // (it goes on from a log not the original's — its own before a rewind: not told)
	}
	pre, post, cp := parts[1], parts[2], parts[3]
	if size < b.end || size-b.end > forkPostMax || int64(len(post)) < size-b.end || len(post) >= forkPostMax {
		return forkRead{}, false // (said since more than is read, or not read whole)
	}
	if int64(len(pre)) != min(b.end, forkPreMax) {
		return forkRead{}, false
	}
	if csize > forkCopyMax || int64(len(cp)) < csize || len(cp) >= forkCopyMax {
		return forkRead{}, false
	}
	return forkRead{pre: pre, post: post, copy: cp}, true
}

// forkScreenReadCommand: the original's log (written by b.writer, under
// sessions) read before and after b.end, and the copy's own log:
// "ok <the original's log's name> <its size> <the copy's size>\0<before>\0<after>\0<copy>".
func forkScreenReadCommand(sessions, copyLog string, b codexBase) string {
	q := sshx.QuoteShell
	return `s=` + q(sessions) + ` w=` + q(b.writer) + ` e=` + strconv.FormatInt(b.end, 10) + ` c=` + q(copyLog) + `
o=
for f in "$s"/*/*/*/rollout-*-"$w".jsonl "$s"/*/*/*/rollout-*_"$w".jsonl; do
  [ -f "$f" ] || continue
  [ -z "$o" ] || exit 0 # (two alike: not guessed between)
  o=$f
done
[ -n "$o" ] || exit 0
sz=$(wc -c <"$o" | tr -d ' ') && cz=$(wc -c <"$c" | tr -d ' ') || exit 0
from=$((e - ` + strconv.Itoa(forkPreMax) + `)); [ "$from" -ge 0 ] || from=0
printf 'ok %s %s %s\0' "${o##*/}" "$sz" "$cz"
tail -c +$((from + 1)) "$o" | head -c $((e - from)) || exit 1
printf '\0'
tail -c +$((e + 1)) "$o" | head -c ` + strconv.Itoa(forkPostMax) + ` || exit 1
printf '\0'
head -c ` + strconv.Itoa(forkCopyMax) + ` "$c" || exit 1`
}

// forkSaidAfter: what the original said after the copy was made (post, its
// log from there; pre, from before), as the screen's letters would show it:
// each part's start and end (a long one shown only in part, its end the
// most), those of some length not said before as well.
func forkSaidAfter(pre, post string) []string {
	text := func(log string) []string {
		// (A part read from a line's middle: from the first line whole.)
		if i := strings.IndexByte(log, '\n'); i >= 0 && !strings.HasPrefix(log, "{") {
			log = log[i+1:]
		}
		c := convo.NewCodex()
		c.Feed([]byte(log))
		var out []string
		for _, e := range c.Since(0) {
			if e.Gone {
				continue
			}
			for _, part := range e.Parts {
				if part.Kind == convo.KindText && part.Tool == nil {
					if w := saidLetters(part.Text); w != "" {
						out = append(out, w)
					}
				}
			}
		}
		return out
	}
	before := strings.Join(text(pre), "\x00")
	var out []string
	for _, w := range text(post) {
		r := []rune(w)
		pieces := []string{w}
		if len(r) > forkPiece {
			pieces = []string{string(r[:forkPiece]), string(r[len(r)-forkPiece:])}
		}
		for _, piece := range pieces {
			if len([]rune(piece)) >= 8 && !strings.Contains(before, piece) {
				out = append(out, piece)
			}
		}
	}
	return out
}

// forkPiece: how much of a part's start and end is looked for on the screen.
const forkPiece = 30
