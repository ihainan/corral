package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

// "/model haiku" saves its word as the default of new sessions, every
// time. A switch from here is of one session: the default is put back as
// it was. What is kept for that, and why:
//
//   - One switch at a time for an account on a host (settingsBusy): two
//     at once would each take the other's word for the default as it was.
//     The account is what the host itself says it is (settingsAccount):
//     two records here of one account share its settings, and two hosts
//     reached by the same address through different jumps do not.
//   - What was there before, what the line writes and where it was typed
//     are kept in the store before the line is typed (SettingsPending).
//   - That is let go only once the line is known to be over and the
//     default is seen settled — not after so many looks. A line is over
//     when its log says how it ended (taken, or its question left), when
//     its process is gone, or when it never ran (no record of it, the box
//     empty, nothing asked). Until then its question may stand open in
//     the terminal, or the line sit in the box, and whoever answers or
//     sends it later would change the default with no one to put it
//     back: so the account's next switch looks again first, and is
//     refused while the line is still open.
//   - Put back only where the default is the word the line wrote. Any
//     other value is someone else's, later than ours: left alone, and
//     with it this switch's say over the default is over.
//
// Not kept from: a write to the settings by someone else between the
// write back's last check and its rename (settingsWriteCommand: a moment
// on the host; Claude Code takes no lock another could share).

// settleIO is the settings as settleDefault needs them.
type settleIO struct {
	read  func() (doc []byte, none bool, err error)
	write func(was, now []byte) (conflict bool, err error) // only if they are still was
}

type settled int

const (
	settleSame    settled = iota // as before the switch: nothing to do
	settleBack                   // the line's word was there: put back
	settleForeign                // neither: someone else's, left alone
	settleFailed                 // not known, or not put back
)

// settleDefault puts the default back as it was (was, absent where had is
// false) where it is what the line wrote (wrote).
func settleDefault(sio settleIO, had bool, was, wrote json.RawMessage) (settled, error) {
	res, _, _, err := settleLook(sio, had, was, wrote)
	return res, err
}

// settleLook is settleDefault, with what the default is where it is
// someone else's (has false: none).
func settleLook(sio settleIO, had bool, was, wrote json.RawMessage) (res settled, cur json.RawMessage, has bool, err error) {
	for try := 0; try < 3; try++ { // (changed between the read and the write: read again)
		doc, none, err := sio.read()
		if err != nil {
			return settleFailed, nil, false, err
		}
		cur, has, err := settingsModel(doc, none)
		if err != nil {
			return settleFailed, nil, false, err
		}
		switch {
		case has == had && (!has || jsonSame(cur, was)):
			return settleSame, nil, false, nil
		case !has || len(wrote) == 0 || !jsonSame(cur, wrote):
			return settleForeign, cur, has, nil
		}
		fixed, err := withModel(doc, was, had)
		if err != nil {
			return settleFailed, nil, false, err
		}
		conflict, err := sio.write(doc, fixed)
		if conflict {
			continue
		}
		if err != nil {
			return settleFailed, nil, false, err
		}
		return settleBack, nil, false, nil
	}
	return settleFailed, nil, false, errors.New("the settings kept changing")
}

// lineProbe looks, without touching anything, at where a line was typed.
type lineProbe struct {
	logFrom func(off int64) ([]byte, error) // its log from there on (some of it)
	alive   func() (alive, known bool)      // Claude Code's process is still that one
	atRest  func() bool                     // its pane shows it at rest, the box empty, nothing asked
}

// lineWatch: a line whose end is looked for, between looks.
type lineWatch struct {
	had        bool
	was, wrote json.RawMessage
	log        *lineLog
	off        int64     // its log read up to here
	typedAt    time.Time // (about: before it was typed)
}

const (
	// lineNeverAge: a line with no record of it is taken for never run
	// only this long after it was typed (its record comes within 0.35 s).
	lineNeverAge = 3 * time.Second
	// lineSettleAge: a line that ran, the default not (yet) its word, is
	// taken for settled only this long after: Claude Code was once found
	// to write the default later than its log's record (out-e14).
	lineSettleAge = 4 * time.Second
)

// lineConclude looks for the line's end and puts the default back where
// it is the line's word. final: the line is over and the default settled
// — nothing more is owed, what was kept of it can go. Not final where it
// may yet be answered or sent (end lineOpen), where it ran a moment ago
// and has not written its word, or where the settings could not be read
// or written — nor, while it is open, for the default having been its
// word and put back: the settings do not say whose word it was. rebased: the default is someone else's while the line is
// still open — it is theirs that is to be put back, should the line yet
// write its word (w says so now, to be kept).
func lineConclude(p lineProbe, sio settleIO, w *lineWatch, now time.Time) (end lineEnd, res settled, final, rebased bool, err error) {
	w.log.read(p.logFrom, &w.off)
	if w.log.end == lineOpen {
		if alive, known := p.alive(); known && !alive {
			end = lineGone
		} else if len(w.log.cmds) == 0 && now.Sub(w.typedAt) >= lineNeverAge && p.atRest() {
			// (At rest, and still no record of it once looked for again.)
			if w.log.read(p.logFrom, &w.off); w.log.end == lineOpen && len(w.log.cmds) == 0 {
				end = lineNever
			}
		}
	}
	if w.log.end != lineOpen {
		end = w.log.end
	}
	res, cur, has, err := settleLook(sio, w.had, w.was, w.wrote)
	switch {
	case res == settleFailed:
	case end == lineOpen:
		// (Not over: kept, whatever the default is. Its word there and put
		// back says nothing of the line — another session may have set the
		// same word — and the line may yet write it.)
		if res == settleForeign {
			w.had, w.was, rebased = has, cur, true
		}
	case res == settleSame && end == lineRan && !(w.had && jsonSame(w.was, w.wrote)):
		final = now.Sub(w.typedAt) >= lineSettleAge
	default:
		final = true
	}
	return end, res, final, rebased, err
}

// settleSSH: the settings of the session's account on its host.
func settleSSH(ctx context.Context, sess *term.Session, running *sync.WaitGroup) settleIO {
	return settleIO{
		read: func() ([]byte, bool, error) { return readSettings(ctx, sess) },
		write: func(was, now []byte) (bool, error) {
			running.Add(1)
			_, err := sshx.Exec(ctx, sess.Client(), settingsWriteCommand(was, now), 4096, sshx.ExecOptions{Stdin: bytes.NewReader(now), Release: running.Done})
			var e *sshx.ExitError
			if errors.As(err, &e) && e.Status == 9 {
				return true, nil
			}
			return false, err
		},
	}
}

// lineExec runs one of the line's fixed commands on the host.
func lineExec(ctx context.Context, sess *term.Session, running *sync.WaitGroup, cmd string, stdin io.Reader, limit int) ([]byte, error) {
	running.Add(1)
	res, err := sshx.Exec(ctx, sess.Client(), cmd, limit, sshx.ExecOptions{Stdin: stdin, Release: running.Done})
	return res.Out, err
}

// lineAliveCommand: exit 0 where the process pid is still the one started
// at start (as the gate's front() tells), 3 where it is not, 4 where it
// cannot be told (no /proc).
func lineAliveCommand(pid, start int64) string {
	script := `[ -r /proc/self/stat ] || exit 4
stat=$(tr '\n' ' ' <"/proc/$1/stat" 2>/dev/null) || exit 3
rest=${stat##*) }
[ "$(printf '%s' "$rest" | cut -d' ' -f20)" = "$2" ] || exit 3`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + strconv.FormatInt(pid, 10) + " " + strconv.FormatInt(start, 10)
}

// lineProbeOf: the probe of a line typed where p says, looked at through
// sess (a session of the same account: the line's own, or another's).
func (s *Server) lineProbeOf(ctx context.Context, sess *term.Session, p *store.SettingsPending, running *sync.WaitGroup) lineProbe {
	return lineProbe{
		logFrom: func(off int64) ([]byte, error) { return s.logFrom(ctx, sess, p.Log, off) },
		alive: func() (bool, bool) {
			_, err := lineExec(ctx, sess, running, lineAliveCommand(p.PID, p.Start), nil, 256)
			var e *sshx.ExitError
			switch {
			case err == nil:
				return true, true
			case errors.As(err, &e) && e.Status == 3:
				return false, true
			}
			return false, false
		},
		atRest: func() bool {
			// (Its screen only where that process is still the one in front in
			// that pane — the gate's front(), exit 3 where it is not: another
			// Claude Code started there, the first put aside, shows a box
			// that is not this line's.)
			out, err := lineExec(ctx, sess, running, convoGatedCommand("", p.Mode, p.TmuxName, p.Pane, p.Socket, p.PID, p.Start, p.SID, 0, []string{}, pickerLines), nil, 64<<10)
			if err != nil {
				return false
			}
			shown := string(out)
			if strings.TrimSpace(shown) == "" || pickerShows(shown) || lineAsks(shown, "") {
				return false
			}
			return idleInput(shown, func() string {
				// (As greyedSuggestion, of the pane kept: by its own mode, which
				// may not be this session's.)
				raw, err := lineExec(ctx, sess, running, convoColorScreenCommand(p.Mode, p.Pane, p.Socket), nil, 512<<10)
				if err != nil || claudeOwner(sgr.ReplaceAllString(string(raw), ""), false) != "main" {
					return ""
				}
				return greyedIn(string(raw))
			}) == nil
		},
	}
}

// watchOf: a line kept in the store, to look for its end from its log's
// length before it.
func watchOf(p *store.SettingsPending) *lineWatch {
	var arg string
	json.Unmarshal(p.Wrote, &arg)
	w := &lineWatch{had: p.Had, wrote: p.Wrote, log: &lineLog{sid: p.SID, arg: arg}, off: p.LogOff, typedAt: time.Unix(p.CreatedAt, 0)}
	if p.Had {
		w.was = p.Was
	}
	return w
}

// pendingMem: what the store could not be told of an account's line — kept
// as it is now (the default to put back became someone else's), or let go
// (nil: over, its row still there). The store's row is then out of date:
// gone by it, the next switch would put back a default no longer the one
// to keep, or take a default set since for the line's word.
var pendingMem sync.Map

func (s *Server) keepPending(ctx context.Context, p store.SettingsPending) error {
	err := s.store.PutSettingsPending(ctx, p)
	if err != nil {
		pendingMem.Store(p.Account, &p)
	} else {
		pendingMem.Delete(p.Account)
	}
	return err
}

func (s *Server) dropPending(ctx context.Context, account string) error {
	err := s.store.DeleteSettingsPending(ctx, account)
	if err != nil {
		pendingMem.Store(account, (*store.SettingsPending)(nil))
	} else {
		pendingMem.Delete(account)
	}
	return err
}

// pendingOf: what is kept of the account's line, nil when nothing.
func (s *Server) pendingOf(ctx context.Context, account string) (*store.SettingsPending, error) {
	v, ok := pendingMem.Load(account)
	if !ok {
		return s.store.SettingsPendingOf(ctx, account)
	}
	p := v.(*store.SettingsPending)
	if p == nil {
		s.dropPending(ctx, account) // (tried again; over all the same)
		return nil, nil
	}
	cp := *p
	s.keepPending(ctx, cp)
	return &cp, nil
}

// settingsBusy: the accounts (settingsAccount) a switch is changing the
// settings of, or still looking for its line's end.
var settingsBusy sync.Map

// settingsAccounts: a session's account, once asked of its host.
var settingsAccounts sync.Map

// settingsIdentCommand prints what the settings are those of: the machine
// (its id; its name only where it has none — a name may be changed under
// what is kept), the user, and the settings' file (through a link, the
// file it names).
//
// Not told apart: machines copied from one another with its id kept, the
// same user and home on each. Not known to be one: a home shared by two
// machines. (Neither can be told from what a host says of itself.)
const settingsIdentCommand = `m=$(cat /etc/machine-id 2>/dev/null)
[ -n "$m" ] || m=name:$(hostname 2>/dev/null || cat /proc/sys/kernel/hostname 2>/dev/null)
f="$HOME/.claude/settings.json"
if [ -L "$f" ]; then f=$(readlink -f "$f") && [ -n "$f" ] || exit 5; fi
u=$(id -u) && [ -n "$u" ] && [ "$m" != name: ] || exit 5
printf '%s\n%s\n%s\n' "$m" "$u" "$f"`

// settingsAccount names whose settings a session's are, as its host says:
// not by the address it is reached by (the same through two jumps is two
// machines) nor the host's record here (which may be changed under a
// session still open).
func settingsAccount(ctx context.Context, sess *term.Session) (string, error) {
	if v, ok := settingsAccounts.Load(sess.ID); ok {
		return v.(string), nil
	}
	out, err := sshx.Output(ctx, sess.Client(), settingsIdentCommand, 8192)
	if err != nil {
		return "", err
	}
	if strings.Count(string(out), "\n") != 3 {
		return "", errors.New("the host did not say whose settings these are")
	}
	sum := sha256.Sum256(out)
	account := "v2:" + hex.EncodeToString(sum[:20])
	settingsAccounts.Store(sess.ID, account)
	return account, nil
}

// defaultFlags: the session told its default was left changed, by the
// account it was left changed for: taken back by whichever switch of the
// account settles it.
var defaultFlags sync.Map

func flagDefault(account, session string) {
	defaultChanged.Store(session, time.Now())
	defaultFlags.Store(account, session)
}

func unflagDefault(account string) {
	if v, ok := defaultFlags.LoadAndDelete(account); ok {
		defaultChanged.Delete(v.(string))
	}
}

const settingsWait = 1500 * time.Millisecond // (a switch that goes well has the settings for about a second)

// settingsTurn takes the account's settings for a switch — waiting a
// moment for one under way, no longer: the pane's turn is held meanwhile —
// and looks first for the end of a line an earlier switch left open.
// ok false: not now (code, why); else done gives them up.
func (s *Server) settingsTurn(ctx context.Context, sess *term.Session, account string) (done func(), code, why string, ok bool) {
	until := time.Now().Add(settingsWait)
	for {
		if _, held := settingsBusy.LoadOrStore(account, struct{}{}); !held {
			break
		}
		if !time.Now().Before(until) {
			return nil, "default_settling", "an earlier switch is still settling", false
		}
		select {
		case <-ctx.Done():
			return nil, "default_settling", "given up", false
		case <-time.After(100 * time.Millisecond):
		}
	}
	var once sync.Once
	done = func() { once.Do(func() { settingsBusy.Delete(account) }) }
	p, err := s.pendingOf(ctx, account)
	if err != nil {
		done()
		return nil, "default_settling", "what an earlier switch left could not be read", false
	}
	if p == nil {
		return done, "", "", true
	}
	var running sync.WaitGroup
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	w := watchOf(p)
	end, res, final, rebased, err := lineConclude(s.lineProbeOf(rctx, sess, p, &running), settleSSH(rctx, sess, &running), w, time.Now())
	cancel()
	running.Wait()
	if rebased {
		p.Had, p.Was = w.had, w.was
		if err := s.keepPending(ctx, *p); err != nil {
			slog.Error("model switch: a default to put back could not be kept", "session", sess.ID, "err", err)
		}
	}
	if !final {
		done()
		switch {
		case res == settleFailed:
			slog.Error("model switch: an earlier switch's default is still not put back", "session", sess.ID, "err", err)
			return nil, "default_settling", "the default model an earlier switch changed could not be put back yet", false
		case end == lineOpen:
			return nil, "default_pending", "an earlier switch's command has not ended (pane " + p.Pane + "): its question may be open in the terminal, or the command still in the input box", false
		}
		return nil, "default_settling", "an earlier switch is still settling", false
	}
	if res == settleBack {
		slog.Warn("model switch: the default an earlier switch left changed; put back", "session", sess.ID)
	}
	if err := s.dropPending(ctx, account); err != nil {
		// (Over all the same: pendingMem says so, whatever the store still holds.)
		slog.Error("model switch: a default to put back could not be let go", "session", sess.ID, "err", err)
	}
	unflagDefault(account)
	return done, "", "", true
}

// lineChecks: when a line not known to be over, or its default not
// settled, is looked at again after the answer was given (each after the
// one before). The pane's turn is kept for the first lineHeld of them
// while the line is open, to leave its question should it come late.
// After them what is kept stays in the store, for the account's next
// switch to look again.
var lineChecks = []time.Duration{2 * time.Second, 4 * time.Second, 9 * time.Second, 15 * time.Second}

const lineHeld = 2

// convoModelLine switches a Claude Code's model with one line (see
// convo_line.go) and puts the default back. It holds the pane's turn
// (release) and the account's settings (done), and gives both back itself.
func (s *Server) convoModelLine(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, target, arg, want, log, account string, outcome, reason *string, release, done func()) {
	var running sync.WaitGroup // (commands that may still run on the host)
	bg := context.WithoutCancel(ctx)
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	saved, none, err := readSettings(sctx, sess)
	was, had, perr := settingsModel(saved, none)
	var off int64
	if err == nil && perr == nil {
		off, err = s.logSize(sctx, sess, log)
	}
	scancel()
	if err != nil || perr != nil {
		done()
		release()
		*reason = "settings"
		writeError(w, http.StatusBadGateway, "settings", "Claude Code's settings or its log could not be read: nothing done")
		return
	}
	wrote, _ := json.Marshal(arg)
	rec := store.SettingsPending{Account: account, OwnerID: sess.OwnerID, HostID: sess.HostID, Had: had, Was: was, Wrote: wrote,
		Mode: sess.Persistence, TmuxName: sess.TmuxSession, Pane: pane, Socket: socket, PID: pid, Start: start, SID: sid, Log: log, LogOff: off, CreatedAt: time.Now().Unix()}
	if err := s.store.PutSettingsPending(ctx, rec); err != nil {
		done()
		release()
		*reason = "store"
		writeError(w, http.StatusInternalServerError, "internal_error", "nothing done")
		return
	}
	watch := &lineWatch{had: had, was: was, wrote: wrote, log: &lineLog{sid: sid, arg: arg}, off: off, typedAt: time.Now()}
	mode := sess.Persistence
	host := func(ctx context.Context) lineIO {
		return lineIO{
			look: func() (string, error) {
				out, err := lineExec(ctx, sess, &running, convoScreenCommand(mode, pane, socket), nil, 512<<10)
				if err != nil {
					return "", err
				}
				shown, _, _ := screenOf(out)
				return shown, nil
			},
			typed: func(text string) error {
				_, err := lineExec(ctx, sess, &running, convoGatedCommand("free", mode, sess.TmuxSession, pane, socket, pid, start, sid, len(text), nil, 0), strings.NewReader(text), 4096)
				return err
			},
			dialog: func() (string, string, error) {
				out, err := lineExec(ctx, sess, &running, convoColorScreenCommand(mode, pane, socket), nil, 512<<10)
				if err != nil {
					return "", "", err
				}
				text := strings.ReplaceAll(string(panelSGR.ReplaceAll(out, nil)), "\r\n", "\n")
				return text, panelDigest(out), nil
			},
			pressOn: func(digest, key string) error {
				k := modelKeys[key][1]
				if mode == "herdr" {
					k = modelKeys[key][0]
				}
				_, err := lineExec(ctx, sess, &running, convoGatedCommand("panel:"+digest, mode, sess.TmuxSession, pane, socket, pid, start, sid, 0, []string{k}, 0), nil, 4096)
				return err
			},
			logFrom: func(off int64) ([]byte, error) { return s.logFrom(ctx, sess, log, off) },
			now:     time.Now,
			sleep: func(d time.Duration) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(d):
					return nil
				}
			},
		}
	}
	// over: nothing more is owed for this line.
	over := func() {
		if err := s.dropPending(bg, account); err != nil {
			slog.Error("model switch: a default to put back could not be let go", "session", sess.ID, "err", err)
		}
		unflagDefault(account)
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	name, sent, err := modelLineFlow(host(fctx), watch.log, &watch.off, want, func() string { return s.greyedSuggestion(fctx, sess, pane, socket, &running) })
	cancel()
	if sent == lineNotSent {
		go func() {
			running.Wait()
			over()
			done()
			release()
		}()
		writeSwitchError(w, err, false, outcome, reason)
		return
	}
	conclude := func() (lineEnd, settled, bool, error) {
		running.Wait()
		cctx, ccancel := context.WithTimeout(bg, 15*time.Second)
		defer ccancel()
		end, res, final, rebased, err := lineConclude(s.lineProbeOf(cctx, sess, &rec, &running), settleSSH(cctx, sess, &running), watch, time.Now())
		running.Wait()
		if rebased {
			rec.Had, rec.Was = watch.had, watch.was
			if err := s.keepPending(bg, rec); err != nil {
				slog.Error("model switch: a default to put back could not be kept", "session", sess.ID, "err", err)
			}
			slog.Warn("model switch: the default was changed by someone else while the line is open; theirs is the one to keep", "session", sess.ID)
		}
		if res == settleFailed {
			slog.Error("model switch: the default model could not be put back (yet)", "session", sess.ID, "err", err)
			flagDefault(account, sess.ID)
		}
		return end, res, final, err
	}
	end, res, final, _ := conclude()
	// (How it ended, found by now where the flow had not: its log's word.)
	if err != nil && end == lineRan {
		if name, err = watch.log.name, nil; name != want {
			err = errElsewhere
		}
	}
	if err != nil && end == lineKept {
		err = errNotTaken
	}
	// (Not said by its log, and the default was the line's word: it ran.)
	unconfirmed := errors.Is(err, errUnsure) && res == settleBack
	go func(end lineEnd, res settled, final bool) { // (its own: the answer below goes by the first look)
		held := true
		free := func() {
			if held {
				held = false
				running.Wait()
				release()
			}
		}
		if end != lineOpen || final {
			free()
		}
		for i, d := range lineChecks {
			if final {
				break
			}
			time.Sleep(d)
			if end == lineOpen && held {
				// (Its question come late, for the model asked for: left, as in
				// the flow.)
				lctx, lcancel := context.WithTimeout(bg, 8*time.Second)
				h := host(lctx)
				if text, digest, err := h.dialog(); err == nil && lineAsks(text, want) {
					h.pressOn(digest, "esc")
				}
				lcancel()
			}
			end, res, final, _ = conclude()
			if i >= lineHeld-1 || end != lineOpen {
				free()
			}
		}
		free()
		if final {
			over()
		} else {
			// (Kept in the store: the account's next switch looks again first.)
			slog.Warn("model switch: a line's end or its default is still open; kept for the next switch", "session", sess.ID, "end", int(end), "settled", int(res))
		}
		done()
	}(end, res, final)
	switch {
	case res == settleFailed:
		// (Above all else: the person is to know. It is tried again for a while.)
		*outcome, *reason = "unknown", "default_not_restored"
		writeError(w, http.StatusInternalServerError, "default_changed", "the default model of new sessions changed and could not be put back")
	case err == nil || unconfirmed:
		*outcome = "typed"
		if unconfirmed {
			*reason = "line_unconfirmed"
		}
		// (restored: said of a default found changed by accident — here it
		// is how the line works, each time.)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": target, "restored": false, "restore_failed": false, "unconfirmed": unconfirmed})
	case errors.Is(err, errElsewhere):
		*outcome, *reason = "typed", "line_elsewhere"
		writeError(w, http.StatusConflict, "model_elsewhere", "it took another model than the one asked for: "+name)
	case errors.Is(err, errUnsure):
		*outcome, *reason = "unknown", "line_unsure"
		writeError(w, http.StatusBadGateway, "unknown", "it is not known whether the model was taken")
	default:
		writeSwitchError(w, err, true, outcome, reason)
	}
}
