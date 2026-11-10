package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// A model of Claude Code's own taken with one line, "/model haiku" and
// Enter, in place of its picker walked with the arrows: no picker to read
// off a screen, and what it took is said by its log, not by the screen.
//
// Measured (Claude Code 2.1.312 and 2.1.321, 2026-11-10): the line switches
// the session at once, opens no picker, writes the settings' "model" (the
// default of new sessions: the word typed, in small letters) within 0.13 s
// and to its log, within 0.35 s, the command and then its answer, the
// answer's parent the command's record:
//
//	<command-name>/model</command-name> … <command-args>haiku</command-args>
//	<local-command-stdout>Set model to `Haiku 5.5` and saved as your default for new sessions</local-command-stdout>
//
// With turns behind it, it may ask "Switch model?" first (as the picker's
// "s" does). A name it does not know is taken all the same and becomes the
// session's model, so only its own four words are ever typed, and only for
// a model its own picker listed for this very process.
//
// The default it saves is put back: convo_line_settle.go.

// lineModels: the words "/model" takes on one line, each tried (out-e14),
// by the picker's name for them.
var lineModels = map[string]string{"Opus": "opus", "Sonnet": "sonnet", "Haiku": "haiku", "Fable": "fable"}

var lineVersioned = regexp.MustCompile(`^[A-Z][a-z]+ \d+(\.\d+)*$`)

// lineOffered: what a process's picker listed, when: the name each of
// lineModels stood for there ("Haiku" → "Haiku 5.5").
type lineOffered struct {
	at    time.Time
	names map[string]string
}

var lineLists sync.Map // (by lineListKey)

const lineListFor = 10 * time.Minute

// lineListKey names a process of Claude Code in a session of ours.
func lineListKey(session string, pid, start int64) string {
	return session + "\x00" + strconv.FormatInt(pid, 10) + "\x00" + strconv.FormatInt(start, 10)
}

// noteOffered keeps what the picker just listed for that process: of its
// rows, those named as lineModels names them, whose description begins
// with that name and a version ("Haiku 5.5 · Fastest…").
func noteOffered(key string, opts []modelOpt, now time.Time) {
	names := map[string]string{}
	for _, o := range opts {
		if _, ok := lineModels[o.Label]; !ok {
			continue
		}
		name, _, _ := strings.Cut(o.Desc, " · ")
		name = strings.TrimSpace(name)
		if strings.HasPrefix(name, o.Label+" ") && lineVersioned.MatchString(name) {
			names[o.Label] = name
		}
	}
	lineLists.Store(key, lineOffered{at: now, names: names})
}

// lineModelArg: the word to type for label, and the name the log is to say
// then, where the picker listed it for that process not long ago; ok false
// otherwise (the picker's own flow is the way then).
func lineModelArg(key, label string, now time.Time) (arg, name string, ok bool) {
	v, found := lineLists.Load(key)
	if !found {
		return "", "", false
	}
	l := v.(lineOffered)
	if now.Sub(l.at) > lineListFor {
		lineLists.Delete(key)
		return "", "", false
	}
	if name, ok = l.names[label]; !ok {
		return "", "", false
	}
	return lineModels[label], name, true
}

var (
	lineCmd  = regexp.MustCompile(`^<command-name>/model</command-name>`)
	lineArgs = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
	lineSaid = regexp.MustCompile("^<local-command-stdout>Set model to `([^`\n]{1,80})`")
)

// lineLogSkip: a record longer than this is passed over, not waited for
// whole (the command's and its answer's are a few hundred bytes).
const lineLogSkip = 64 << 10

// lineLog reads the log as it grows for the answer to "/model arg" typed
// into session sid: the command's own record, then the answer whose parent
// that record is. The same words quoted in a message, an answer to another
// command, or one of another session writing to the same file are not it.
type lineLog struct {
	sid, arg string
	skip     bool            // within a record passed over, its end not yet read
	cmds     map[string]bool // uuids of the command's records
}

// feed takes what was read from where the last feed's use ended, and gives
// how much of it is done with (read on from there) and the name the answer
// gives, when it has come.
func (l *lineLog) feed(data []byte) (used int, name string, found bool) {
	if l.skip {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return len(data), "", false
		}
		l.skip, used = false, i+1
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < used {
		if len(data)-used >= lineLogSkip {
			l.skip = true
			return len(data), "", false
		}
		return used, "", false
	}
	for _, row := range bytes.Split(data[used:end], []byte("\n")) {
		if !bytes.Contains(row, []byte("command-name")) && !bytes.Contains(row, []byte("local-command-stdout")) {
			continue
		}
		var rec struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			UUID      string `json:"uuid"`
			Parent    string `json:"parentUuid"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		var text string
		if json.Unmarshal(row, &rec) != nil || rec.Type != "user" || rec.SessionID != l.sid || json.Unmarshal(rec.Message.Content, &text) != nil {
			continue
		}
		if lineCmd.MatchString(text) {
			if m := lineArgs.FindStringSubmatch(text); m != nil && strings.EqualFold(strings.TrimSpace(m[1]), l.arg) && rec.UUID != "" {
				if l.cmds == nil {
					l.cmds = map[string]bool{}
				}
				l.cmds[rec.UUID] = true
			}
			continue
		}
		if m := lineSaid.FindStringSubmatch(text); m != nil && l.cmds[rec.Parent] {
			name, found = m[1], true
		}
	}
	return end + 1, name, found
}

// lineIO is the host as modelLineFlow needs it.
type lineIO struct {
	look    func() (string, error)                  // the screen as it is
	typed   func(text string) error                 // a line typed and Enter, not while it waits for an answer
	dialog  func() (text, digest string, err error) // the screen, and what names it for pressOn
	pressOn func(digest, key string) error          // a key of modelKeys, only on that screen still
	logSize func() (int64, error)
	logFrom func(off int64) ([]byte, error) // the log from there on (some of it)
	now     func() time.Time
	sleep   func(time.Duration) error // (an error: given up)
}

// lineAsks: its question is on the screen, for target ("" any) — in a pane
// as narrow as they come too: the picker's flow is not gone through where
// the pane is that narrow, this one is.
func lineAsks(screen, target string) bool { return askWithin(screen, target, lineAskSpan) }

const lineAskSpan = 16

var errElsewhere = errors.New("it took another model than the one asked for")

const (
	lineLogEvery = 100 * time.Millisecond
	lineLogFirst = 1 * time.Second // its record is there within 0.35 s where it asks nothing
	lineLogAfter = 2 * time.Second // after its question was answered
)

// lineSent: what became of the line.
type lineSent int

const (
	lineNotSent lineSent = iota // nothing reached Claude Code
	lineMaybe                   // it may have (typed, or the host's answer lost): the default is watched
)

// lineNotTyped: the error of typing a line says nothing was typed (the
// gate's refusals before its paste, see convoGated). Any other — typed and
// its Enter held back (5, 16), cut off while typing (6), the host's answer
// lost or late — may have reached Claude Code.
func lineNotTyped(err error) bool {
	if errors.Is(err, sshx.ErrBusy) {
		return true
	}
	var e *sshx.ExitError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case 3, 4, 7, 8, 9, 10, 11, 12, exitOwnerSubagent, exitOwnerOther, exitOwnerNone, 17:
		return true
	}
	return false
}

// modelLineFlow types "/model arg" into a Claude Code at rest with its
// input empty (as modelFlow asks), and waits for its log to say it took
// the model named want. nil: it did. errElsewhere with the name: it took
// another. errUnsure: the line may have been typed and the log did not say
// in time. errNotTaken: its question was left unanswered with Escape (it
// was for another model, or it was still there when the wait was over).
// Before the line is typed, the errors of idleInput and errScreen. sent
// says whether the default may change by it, whatever the error.
func modelLineFlow(io lineIO, sid, arg, want string, greyed ...func() string) (name string, sent lineSent, err error) {
	screen, err := io.look()
	if err != nil {
		return "", lineNotSent, err
	}
	// (Its picker or its question left open by someone: not closed from
	// here — the picker's own flow does that, and is not this one.)
	if pickerShows(screen) || lineAsks(screen, "") {
		return "", lineNotSent, errScreen
	}
	if err := idleInput(screen, greyed...); err != nil {
		return "", lineNotSent, err
	}
	off, err := io.logSize()
	if err != nil {
		return "", lineNotSent, err
	}
	if err := io.typed("/model " + arg); err != nil {
		if lineNotTyped(err) {
			return "", lineNotSent, err
		}
		return "", lineMaybe, err
	}
	log := lineLog{sid: sid, arg: arg}
	wait := func(d time.Duration) (string, bool) {
		until := io.now().Add(d)
		for {
			if data, err := io.logFrom(off); err == nil {
				used, name, ok := log.feed(data)
				off += int64(used)
				if ok {
					return name, true
				}
			}
			if !io.now().Before(until) || io.sleep(lineLogEvery) != nil {
				return "", false
			}
		}
	}
	// leave: its question, where it is (still) on the screen, left with
	// Escape — on the screen it was read off only — so that it is not
	// answered later, by whoever, the default then changing with no one
	// watching. Whether it was left.
	leave := func() bool {
		text, digest, err := io.dialog()
		return err == nil && lineAsks(text, "") && io.pressOn(digest, "esc") == nil
	}
	name, ok := wait(lineLogFirst)
	if !ok {
		// (Nothing said: its question, "Switch model?", may be what it waits
		// on. Enter only on that question, for the model asked for, and only
		// on the screen it was read off.)
		text, digest, err := io.dialog()
		if err != nil {
			return "", lineMaybe, errUnsure
		}
		if lineAsks(text, "") {
			if !lineAsks(text, want) {
				if io.pressOn(digest, "esc") != nil {
					return "", lineMaybe, errUnsure
				}
				return "", lineMaybe, errNotTaken
			}
			if err := io.pressOn(digest, "yes"); err != nil {
				if leave() {
					return "", lineMaybe, errNotTaken
				}
				return "", lineMaybe, errUnsure
			}
		}
		if name, ok = wait(lineLogAfter); !ok {
			if leave() {
				return "", lineMaybe, errNotTaken
			}
			return "", lineMaybe, errUnsure
		}
	}
	if name != want {
		return name, lineMaybe, errElsewhere
	}
	return name, lineMaybe, nil
}

const lineLogMost = 256 << 10 // of the log read at once: its record is short, and soon

// logSize: the log's size now.
func (s *Server) logSize(ctx context.Context, sess *term.Session, log string) (size int64, err error) {
	err = s.withSFTP(ctx, sess, 5*time.Second, func(c *sftplite.Client) error {
		fi, err := c.Stat(log)
		if err != nil {
			return err
		}
		size = fi.Size()
		return nil
	})
	return size, err
}

// logFrom: the log from off on (lineLogMost of it at most).
func (s *Server) logFrom(ctx context.Context, sess *term.Session, log string, off int64) (data []byte, err error) {
	err = s.withSFTP(ctx, sess, 5*time.Second, func(c *sftplite.Client) error {
		fi, err := c.Stat(log)
		if err != nil {
			return err
		}
		n := min(fi.Size()-off, lineLogMost)
		if n <= 0 {
			return nil
		}
		data, err = c.ReadRange(log, off, int(n))
		return err
	})
	return data, err
}
