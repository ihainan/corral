package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/term"
)

// A model of Claude Code's own taken with one line, "/model haiku" and
// Enter, in place of its picker walked with the arrows: no picker to read
// off a screen, and what it took is said by its log, not by the screen.
//
// Measured (Claude Code 2.1.321, 2026-11-10): the line switches the session
// at once, opens no picker, writes the settings' "model" (the default of
// new sessions: the word typed, in small letters) within 0.13 s and its
// log's record within 0.35 s:
//
//	<local-command-stdout>Set model to `Haiku 5.5` and saved as your default for new sessions</local-command-stdout>
//
// With turns behind it, it may ask "Switch model?" first (as the picker's
// "s" does). A name it does not know is taken all the same and becomes the
// session's model, so only its own four words are ever typed.
//
// The default it saves is put back by convoModel's restore, as after the
// picker; here it is known to change every time.

// lineModels: the words "/model" takes on one line, each tried (out-e14).
var lineModels = map[string]bool{"opus": true, "sonnet": true, "haiku": true, "fable": true}

var lineLabel = regexp.MustCompile(`^([A-Z][a-z]+)( \d+(\.\d+)*)?$`)

// lineModelArg: the word to type for a model as the picker names it
// ("Haiku", "Haiku 5.5"), ok false where it is not one of lineModels, or
// names more than a family and its version ("Opus 5.5 (1M context)").
func lineModelArg(label string) (arg string, ok bool) {
	m := lineLabel.FindStringSubmatch(label)
	if m == nil || !lineModels[strings.ToLower(m[1])] {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// lineNameIs: the name the log says was taken is the model label names:
// the label itself, or the label's family with a version after it.
func lineNameIs(name, label string) bool {
	if name == label {
		return true
	}
	return !strings.Contains(label, " ") && strings.HasPrefix(name, label+" ") && lineLabel.MatchString(name)
}

var lineSaid = regexp.MustCompile("^<local-command-stdout>Set model to `([^`\n]{1,80})`")

// lineTook reads whole records of the log (what was written to it since
// the line was typed) for the command's own answer: the model it names.
// Only a record of the person's side whose words are that answer from
// their first letter counts: quoted in a message or in an answer, the same
// words are within other words.
func lineTook(tail []byte) (name string, found bool) {
	for _, row := range bytes.Split(tail, []byte("\n")) {
		if !bytes.Contains(row, []byte("local-command-stdout")) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Content string `json:"content"` // (a "system" record's)
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(row, &rec) != nil || rec.Type != "user" && rec.Type != "system" {
			continue
		}
		text := rec.Content
		if text == "" {
			if json.Unmarshal(rec.Message.Content, &text) != nil {
				continue
			}
		}
		if m := lineSaid.FindStringSubmatch(text); m != nil {
			name, found = m[1], true // (the last of them)
		}
	}
	return name, found
}

// lineIO is the host as modelLineFlow needs it.
type lineIO struct {
	look    func() (string, error)              // the screen as it is
	typed   func(text string) error             // a line typed and Enter, not while it waits for an answer
	press   func(keys []string) (string, error) // keys of modelKeys, then the screen
	logSize func() (int64, error)
	logFrom func(off int64) ([]byte, error) // the log from there on
	sleep   func(time.Duration)
}

var errElsewhere = errors.New("it took another model than the one asked for")

const (
	lineLogEvery = 100 * time.Millisecond
	lineLogFirst = 1 * time.Second // its record is there within 0.35 s where it asks nothing
	lineLogAfter = 2 * time.Second // after its question was answered
)

// modelLineFlow types "/model arg" into a Claude Code at rest with its
// input empty (as modelFlow asks), and waits for its log to say which
// model it took: the name, nil where that is label's. errElsewhere with
// the name where it is another's. errUnsure where the line was typed and
// the log said nothing in time (the caller then looks to the settings, see
// convoModel). Before the line is typed, the errors of idleInput and
// errScreen: nothing done.
func modelLineFlow(io lineIO, label, arg string, greyed ...func() string) (name string, err error) {
	screen, err := io.look()
	if err != nil {
		return "", err
	}
	// (Its picker or its question left open by someone: not closed from
	// here — the picker's own flow does that, and is not this one.)
	if pickerShows(screen) || askShows(screen, "") {
		return "", errScreen
	}
	if err := idleInput(screen, greyed...); err != nil {
		return "", err
	}
	off, err := io.logSize()
	if err != nil {
		return "", err
	}
	if err := io.typed("/model " + arg); err != nil {
		return "", err
	}
	wait := func(d time.Duration) (string, bool) {
		for spent := time.Duration(0); ; spent += lineLogEvery {
			if tail, err := io.logFrom(off); err == nil {
				if name, ok := lineTook(tail); ok {
					return name, true
				}
			}
			if spent >= d {
				return "", false
			}
			io.sleep(lineLogEvery)
		}
	}
	name, ok := wait(lineLogFirst)
	if !ok {
		// (Nothing said: its question, "Switch model?", may be what it waits
		// on. Enter only on that question, its first choice the model's.)
		screen, err := io.press([]string{})
		if err != nil {
			return "", errUnsure
		}
		if askShows(screen, "") {
			if !lineAsks(screen, label) {
				// (For another model than asked: left with Escape, nothing taken.)
				io.press([]string{"esc"})
				return "", errNotTaken
			}
			if _, err := io.press([]string{"yes"}); err != nil {
				return "", errUnsure
			}
		}
		if name, ok = wait(lineLogAfter); !ok {
			return "", errUnsure
		}
	}
	if !lineNameIs(name, label) {
		return name, errElsewhere
	}
	return name, nil
}

// lineAsks: its question is for label's model: "Yes, switch to <name>",
// the name label's or its family with a version.
func lineAsks(screen, label string) bool {
	if askShows(screen, label) {
		return true
	}
	if strings.Contains(label, " ") {
		return false
	}
	// (A family alone: any version of it. The names tried are few: looked
	// for among what a family's version can be.)
	for _, l := range strings.Split(screen, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "❯"))
		if name, ok := strings.CutPrefix(t, "1. Yes, switch to "); ok && lineNameIs(strings.TrimSpace(name), label) && askShows(screen, strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

// settingsTurns: whose turn it is to change a host's settings of Claude
// Code (by the host's id), as convoSending is a pane's.
var settingsTurns = paneTurns{m: map[string]*paneTurn{}}

// lineRechecks: when the default is looked at again after a line was
// typed. Taken and put back: once more, soon (it was once found written
// later than its record: out-e14). Not known to be taken: for half a
// minute, as it may yet write it.
func lineRechecks(unsure bool) []time.Duration {
	if unsure {
		return []time.Duration{2 * time.Second, 8 * time.Second, 20 * time.Second}
	}
	return []time.Duration{2 * time.Second}
}

const lineLogMost = 256 << 10 // of the log read for its answer: its record is short, and soon

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
