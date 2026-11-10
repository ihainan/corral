package web

import (
	"bytes"
	"context"
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
//     By the account, not by the host's record here: two records of one
//     account share its settings.
//   - What was there before and what the line writes are kept in the
//     store before the line is typed (store.SettingsPending), and let go
//     only once the default is seen settled. A gateway restarted in
//     between, or a host not answering, loses nothing: the next switch
//     for the account settles it first, or is refused.
//   - Put back only where the default is the word the line wrote. Any
//     other value is someone else's, later than ours: left alone.
//   - Looked at again after the answer was given: Claude Code was once
//     found to write it later than its log's record (out-e14), and where
//     it is not known whether the line was taken, it may yet be.

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
	for try := 0; try < 3; try++ { // (changed between the read and the write: read again)
		doc, none, err := sio.read()
		if err != nil {
			return settleFailed, err
		}
		cur, has, err := settingsModel(doc, none)
		if err != nil {
			return settleFailed, err
		}
		switch {
		case has == had && (!has || jsonSame(cur, was)):
			return settleSame, nil
		case !has || len(wrote) == 0 || !jsonSame(cur, wrote):
			return settleForeign, nil
		}
		fixed, err := withModel(doc, was, had)
		if err != nil {
			return settleFailed, err
		}
		conflict, err := sio.write(doc, fixed)
		if conflict {
			continue
		}
		if err != nil {
			return settleFailed, err
		}
		return settleBack, nil
	}
	return settleFailed, errors.New("the settings kept changing")
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

// settingsBusy: the accounts (settingsAccount) a switch is changing the
// settings of, or still watching them after.
var settingsBusy sync.Map

// settingsAccount names whose settings a session's are: the account on
// the host, as the host's record has them.
func (s *Server) settingsAccount(ctx context.Context, sess *term.Session) string {
	if h, err := s.store.Host(ctx, sess.OwnerID, sess.HostID); err == nil && h != nil {
		return strings.ToLower(h.Address) + "|" + strconv.Itoa(h.Port) + "|" + h.Username
	}
	return "host|" + strconv.FormatInt(sess.HostID, 10)
}

const settingsWait = 4 * time.Second // (a switch that went well watches for two)

// settingsTurn takes the account's settings for a switch — waiting a
// moment for one before it, not for as long as it may watch — and settles
// what an earlier one left to be put back. ok false: not now (why); else
// done gives them up.
func (s *Server) settingsTurn(ctx context.Context, sess *term.Session, account string) (done func(), why string, ok bool) {
	until := time.Now().Add(settingsWait)
	for {
		if _, held := settingsBusy.LoadOrStore(account, struct{}{}); !held {
			break
		}
		if !time.Now().Before(until) {
			return nil, "an earlier switch is still settling", false
		}
		select {
		case <-ctx.Done():
			return nil, "given up", false
		case <-time.After(100 * time.Millisecond):
		}
	}
	var once sync.Once
	done = func() { once.Do(func() { settingsBusy.Delete(account) }) }
	p, err := s.store.SettingsPendingOf(ctx, account)
	if err != nil {
		done()
		return nil, "what an earlier switch left could not be read", false
	}
	if p == nil {
		return done, "", true
	}
	var running sync.WaitGroup
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	res, err := settleDefault(settleSSH(rctx, sess, &running), p.Had, p.Was, p.Wrote)
	cancel()
	running.Wait()
	if res == settleFailed {
		slog.Error("model switch: an earlier switch's default is still not put back", "session", sess.ID, "err", err)
		done()
		return nil, "the default model an earlier switch changed could not be put back yet", false
	}
	if res == settleBack {
		slog.Warn("model switch: the default an earlier switch left changed; put back", "session", sess.ID)
	}
	if err := s.store.DeleteSettingsPending(ctx, account); err != nil {
		done()
		return nil, "what an earlier switch left could not be let go", false
	}
	return done, "", true
}

// lineChecks: when the default is looked at again after the answer was
// given (each after the one before). The line known taken: once, soon.
// Else for half a minute, as it may yet be taken; the pane's turn is kept
// for the first lineHeld of them, to leave its question should it come.
var (
	lineChecksSure   = []time.Duration{2 * time.Second}
	lineChecksUnsure = []time.Duration{2 * time.Second, 4 * time.Second, 9 * time.Second, 15 * time.Second}
)

const lineHeld = 2

// convoModelLine switches a Claude Code's model with one line (see
// convo_line.go) and puts the default back. It holds the pane's turn
// (release) and the account's settings (done), and gives both back itself.
func (s *Server) convoModelLine(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, target, arg, want, log, account string, outcome, reason *string, release, done func()) {
	var running sync.WaitGroup // (commands that may still run on the host)
	bg := context.WithoutCancel(ctx)
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	saved, none, err := readSettings(sctx, sess)
	scancel()
	was, had, perr := settingsModel(saved, none)
	if err != nil || perr != nil {
		done()
		release()
		*reason = "settings"
		writeError(w, http.StatusBadGateway, "settings", "Claude Code's settings could not be read: nothing done")
		return
	}
	wrote, _ := json.Marshal(arg)
	if err := s.store.PutSettingsPending(ctx, store.SettingsPending{Account: account, OwnerID: sess.OwnerID, HostID: sess.HostID, Had: had, Was: was, Wrote: wrote}); err != nil {
		done()
		release()
		*reason = "store"
		writeError(w, http.StatusInternalServerError, "internal_error", "nothing done")
		return
	}
	exec := func(ctx context.Context, cmd string, stdin io.Reader, limit int) ([]byte, error) {
		running.Add(1)
		res, err := sshx.Exec(ctx, sess.Client(), cmd, limit, sshx.ExecOptions{Stdin: stdin, Release: running.Done})
		return res.Out, err
	}
	mode := sess.Persistence
	host := func(ctx context.Context) lineIO {
		return lineIO{
			look: func() (string, error) {
				out, err := exec(ctx, convoScreenCommand(mode, pane, socket), nil, 512<<10)
				if err != nil {
					return "", err
				}
				shown, _, _ := screenOf(out)
				return shown, nil
			},
			typed: func(text string) error {
				_, err := exec(ctx, convoGatedCommand("free", mode, sess.TmuxSession, pane, socket, pid, start, sid, len(text), nil, 0), strings.NewReader(text), 4096)
				return err
			},
			dialog: func() (string, string, error) {
				out, err := exec(ctx, convoColorScreenCommand(mode, pane, socket), nil, 512<<10)
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
				_, err := exec(ctx, convoGatedCommand("panel:"+digest, mode, sess.TmuxSession, pane, socket, pid, start, sid, 0, []string{k}, 0), nil, 4096)
				return err
			},
			logSize: func() (int64, error) { return s.logSize(ctx, sess, log) },
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
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	name, sent, err := modelLineFlow(host(fctx), sid, arg, want, func() string { return s.greyedSuggestion(fctx, sess, pane, socket, &running) })
	cancel()
	if sent == lineNotSent {
		go func() {
			running.Wait()
			if err := s.store.DeleteSettingsPending(bg, account); err != nil {
				slog.Error("model switch: a default to put back could not be let go", "session", sess.ID, "err", err)
			}
			done()
			release()
		}()
		writeSwitchError(w, err, false, outcome, reason)
		return
	}
	settle := func() (settled, error) {
		running.Wait()
		rctx, rcancel := context.WithTimeout(bg, 15*time.Second)
		defer rcancel()
		res, err := settleDefault(settleSSH(rctx, sess, &running), had, was, wrote)
		running.Wait()
		return res, err
	}
	res, serr := settle()
	sure := err == nil
	// (Not said by its log, and the default was the line's word: it ran.)
	unconfirmed := errors.Is(err, errUnsure) && res == settleBack
	if res == settleFailed {
		slog.Error("model switch: the default model changed and could not be put back", "session", sess.ID, "err", serr)
		defaultChanged.Store(sess.ID, time.Now())
	}
	if res == settleForeign {
		slog.Warn("model switch: the default model was changed by someone else meanwhile; left as it is", "session", sess.ID)
	}
	go func() {
		checks := lineChecksUnsure
		if sure {
			checks = lineChecksSure
			release()
		}
		last := res
		for i, d := range checks {
			time.Sleep(d)
			if !sure && i < lineHeld {
				// (Its question come late, for the model asked for: left, as in
				// the flow — answered later, the default would change unwatched.)
				lctx, lcancel := context.WithTimeout(bg, 8*time.Second)
				h := host(lctx)
				if text, digest, err := h.dialog(); err == nil && lineAsks(text, want) {
					h.pressOn(digest, "esc")
				}
				lcancel()
			}
			if !sure && i == lineHeld-1 {
				running.Wait()
				release()
			}
			r, err := settle()
			if r == settleBack && i > 0 || r == settleFailed {
				slog.Warn("model switch: the default looked at again", "session", sess.ID, "check", i, "result", int(r), "err", err)
			}
			last = r
		}
		if last == settleFailed {
			// (Kept in the store: the account's next switch settles it first.)
			slog.Error("model switch: the default model is still not put back", "session", sess.ID)
			defaultChanged.Store(sess.ID, time.Now())
		} else {
			defaultChanged.Delete(sess.ID)
			if err := s.store.DeleteSettingsPending(bg, account); err != nil {
				slog.Error("model switch: a default to put back could not be let go", "session", sess.ID, "err", err)
			}
		}
		done()
	}()
	switch {
	case res == settleFailed:
		// (Above all else: the person is to know. It is tried again for a while.)
		*outcome, *reason = "unknown", "default_not_restored"
		writeError(w, http.StatusInternalServerError, "default_changed", "the default model of new sessions changed and could not be put back")
	case sure || unconfirmed:
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
