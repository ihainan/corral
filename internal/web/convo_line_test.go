package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/sshx"
)

// fakeLine stands in for a Claude Code taking "/model NAME" on one line,
// as the real one does (out-e10, out-e11, out-e14): any name is taken, its
// log says so a moment later (the command's record, then its answer), and
// with turns behind it, it asks first. It is no more helpful than the real
// one and its host: reads fail, records come late and in pieces, other
// sessions write to the same log, the screen moves under a key.
type fakeLine struct {
	names   map[string]string // what a word typed is the name of
	history bool              // it asks "Switch model?" first
	asksFor string            // (its question names this, not what was typed)
	silent  bool              // its log never says
	lateBy  time.Duration     // its records show this long after it took the model
	chunk   int               // of the log given by one read (0: all)
	failing int               // reads of the log that fail first
	typeErr error             // what typing gives back…
	lands   bool              // …the line having reached it all the same
	moves   int               // presses before which the screen moves (their digest no longer its)
	stuck   bool              // its question stays, whatever is pressed "yes" on it
	givenUp bool              // sleeps fail: the request is over
	draft   string
	busy    bool
	open    bool // a picker left open

	sid     string
	clock   time.Time
	asking  string // the name its question is for
	word    string // (the word that question came of)
	log     []byte
	hidden  []byte // records not yet shown
	showAt  time.Time
	draws   int
	typed   []string
	pressed []string
	session string
}

func newFakeLine() *fakeLine {
	return &fakeLine{sid: "sid-1", clock: time.Unix(1_800_000_000, 0),
		names: map[string]string{"opus": "Opus 5.5", "sonnet": "Sonnet 5.5", "haiku": "Haiku 5.5", "fable": "Fable 5.2"}}
}

func (f *fakeLine) screen() string {
	var b strings.Builder
	if f.busy {
		b.WriteString("✻ Crunching… (12s · esc to interrupt)\n")
	}
	switch {
	case f.asking != "":
		b.WriteString("   Switch model?\n   Your next response will be slower and use more tokens\n\n   ❯ 1. Yes, switch to " + f.asking + "\n     2. No, go back\n")
	default:
		b.WriteString("────────────────────────\n❯ " + f.draft + "\n────────────────────────\n  Opus 5.5 · /tmp\n")
		if f.open {
			b.WriteString("   Select model\n   ❯ 1.  Default (recommended)\n   Enter to set as default · s to use this session only · Esc to cancel\n")
		}
	}
	return b.String()
}

func lineRecord(sid, uuid, parent, content string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "user", "sessionId": sid, "uuid": uuid, "parentUuid": parent,
		"message": map[string]any{"role": "user", "content": content}})
	return append(b, '\n')
}

func lineRecords(sid, uuid, word, name string) []byte {
	cmd := "<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args>" + word + "</command-args>"
	out := "<local-command-stdout>Set model to `" + name + "` and saved as your default for new sessions</local-command-stdout>"
	return append(lineRecord(sid, uuid, "p-"+uuid, cmd), lineRecord(sid, "a-"+uuid, uuid, out)...)
}

func (f *fakeLine) take(word string) {
	name := f.names[word]
	if name == "" {
		name = word // (a name it does not know: taken as it is)
	}
	f.session, f.asking = name, ""
	if !f.silent {
		f.hidden = append(f.hidden, lineRecords(f.sid, fmt.Sprintf("c%d", len(f.typed)), word, name)...)
		f.showAt = f.clock.Add(f.lateBy)
	}
}

func (f *fakeLine) digest() string { return fmt.Sprintf("%d:%x", f.draws, len(f.screen())) }

func (f *fakeLine) io() lineIO {
	return lineIO{
		look: func() (string, error) { return f.screen(), nil },
		typed: func(text string) error {
			if f.typeErr != nil && !f.lands {
				return f.typeErr
			}
			f.typed = append(f.typed, text)
			word, ok := strings.CutPrefix(text, "/model ")
			if !ok {
				panic("not the model's line: " + text)
			}
			if f.history {
				f.word, f.asking = word, f.names[word]
				if f.asksFor != "" {
					f.asking = f.asksFor
				}
			} else {
				f.take(word)
			}
			return f.typeErr
		},
		dialog: func() (string, string, error) { return f.screen(), f.digest(), nil },
		pressOn: func(digest, key string) error {
			if f.moves > 0 {
				f.moves--
				f.draws++
			}
			if digest != f.digest() {
				return &sshx.ExitError{Status: 10}
			}
			if f.asking == "" {
				return &sshx.ExitError{Status: 9} // (no dialog of its own open)
			}
			f.pressed = append(f.pressed, key)
			switch key {
			case "yes":
				if !f.stuck {
					f.take(f.word)
				}
			case "esc":
				f.asking = ""
			}
			f.draws++
			return nil
		},
		logSize: func() (int64, error) { return int64(len(f.log)), nil },
		logFrom: func(off int64) ([]byte, error) {
			if f.failing > 0 {
				f.failing--
				return nil, errors.New("the host did not answer")
			}
			if len(f.hidden) > 0 && !f.clock.Before(f.showAt) {
				f.log, f.hidden = append(f.log, f.hidden...), nil
			}
			data := f.log[min(int(off), len(f.log)):]
			if f.chunk > 0 && len(data) > f.chunk {
				data = data[:f.chunk]
			}
			return data, nil
		},
		now: func() time.Time { return f.clock },
		sleep: func(d time.Duration) error {
			if f.givenUp {
				return errors.New("given up")
			}
			f.clock = f.clock.Add(d)
			return nil
		},
	}
}

func TestLineModelArg(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := lineListKey("s1", 42, 7)
	noteOffered(key, []modelOpt{
		{Label: "Default (recommended)", Desc: "Opus 5.5 · Most capable"},
		{Label: "Haiku", Desc: "Haiku 5.5 · Fastest for quick answers"},
		{Label: "Sonnet", Desc: "Sonnet 5.5 · Best for everyday tasks"},
		{Label: "Opus", Desc: "Something else · no version"},
		{Label: "Fable", Desc: "Opus 5.5 · another's name"},
		{Label: "Opus 5.5 (1M context)", Desc: "Opus 5.5 · long"},
	}, now)
	if arg, name, ok := lineModelArg(key, "Haiku", now.Add(time.Minute)); !ok || arg != "haiku" || name != "Haiku 5.5" {
		t.Fatalf("Haiku: %q %q %v", arg, name, ok)
	}
	for _, label := range []string{"Opus", "Fable", "Opus 5.5 (1M context)", "Default (recommended)", "haiku", "Haiku 5.5", ""} {
		if _, _, ok := lineModelArg(key, label, now); ok {
			t.Errorf("%q: taken for a model to type on one line", label)
		}
	}
	// (Another process, or another session: not listed for it.)
	if _, _, ok := lineModelArg(lineListKey("s1", 43, 7), "Haiku", now); ok {
		t.Error("listed for another process: taken")
	}
	if _, _, ok := lineModelArg(key, "Haiku", now.Add(lineListFor+time.Second)); ok {
		t.Error("listed long ago: taken")
	}
	if _, _, ok := lineModelArg(key, "Sonnet", now.Add(time.Minute)); ok {
		t.Error("a list grown old was not let go")
	}
}

func TestLineLogFeed(t *testing.T) {
	ours := lineRecords("sid-1", "c1", "haiku", "Haiku 5.5")
	feedAll := func(l *lineLog, data []byte, chunk int) (string, bool) {
		off := 0
		for spins := 0; off < len(data) && spins < 10000; spins++ {
			used, name, ok := l.feed(data[off:min(len(data), off+chunk)])
			off += used
			if ok {
				return name, true
			}
			if used == 0 {
				chunk += 7 // (a record not whole yet: more of it next time)
			}
		}
		return "", false
	}
	for _, chunk := range []int{1 << 20, 50, 7} {
		l := lineLog{sid: "sid-1", arg: "haiku"}
		if name, ok := feedAll(&l, ours, chunk); !ok || name != "Haiku 5.5" {
			t.Errorf("chunk %d: %q %v", chunk, name, ok)
		}
	}
	answer := "<local-command-stdout>Set model to `Haiku 5.5` and saved as your default for new sessions</local-command-stdout>"
	not := map[string][]byte{
		"another session's":         lineRecords("sid-2", "c1", "haiku", "Haiku 5.5"),
		"an answer with no command": lineRecord("sid-1", "a1", "c1", answer),
		"another word's command":    lineRecords("sid-1", "c1", "sonnet", "Haiku 5.5"),
		"the answer of another command": append(lineRecord("sid-1", "c1", "p", "<command-name>/model</command-name>\n<command-args>haiku</command-args>"),
			lineRecord("sid-1", "a1", "c0", answer)...),
		"quoted in a message": append(lineRecord("sid-1", "c1", "p", "look: <command-name>/model</command-name><command-args>haiku</command-args>"),
			lineRecord("sid-1", "a1", "c1", "it said "+answer)...),
		"an assistant's": []byte(`{"type":"assistant","sessionId":"sid-1","uuid":"c1","message":{"content":"<command-name>/model</command-name><command-args>haiku</command-args>"}}` + "\n" +
			`{"type":"assistant","sessionId":"sid-1","uuid":"a1","parentUuid":"c1","message":{"content":"` + strings.ReplaceAll(answer, "`", "\\u0060") + `"}}` + "\n"),
		"not whole": ours[:len(ours)-1],
	}
	for what, data := range not {
		l := lineLog{sid: "sid-1", arg: "haiku"}
		if name, ok := feedAll(&l, data, 1<<20); ok {
			t.Errorf("%s: taken for the answer (%q)", what, name)
		}
	}
	// (A record too long to wait for is passed over, what follows it read.)
	long := append(lineRecord("sid-1", "x", "", strings.Repeat("y", 3*lineLogSkip)), ours...)
	l := lineLog{sid: "sid-1", arg: "haiku"}
	if name, ok := feedAll(&l, long, lineLogSkip); !ok || name != "Haiku 5.5" {
		t.Errorf("after a long record: %q %v", name, ok)
	}
}

func TestModelLineFlowSwitches(t *testing.T) {
	for _, c := range []struct {
		what string
		set  func(f *fakeLine)
	}{
		{"at once", func(f *fakeLine) {}},
		{"its record late", func(f *fakeLine) { f.lateBy = 600 * time.Millisecond }},
		{"its log read in pieces", func(f *fakeLine) { f.chunk = 600 }},
		{"reads failing first", func(f *fakeLine) { f.failing = 3 }},
		{"asked first", func(f *fakeLine) { f.history = true }},
		{"asked first, its record late", func(f *fakeLine) { f.history, f.lateBy = true, 1500*time.Millisecond }},
		{"others writing to its log", func(f *fakeLine) {
			f.hidden = append(lineRecords("sid-2", "z1", "haiku", "Opus 5.5"), lineRecords("sid-1", "z2", "sonnet", "Sonnet 5.5")...)
		}},
	} {
		f := newFakeLine()
		f.log = lineRecords("sid-1", "old", "haiku", "Haiku 5.5") // (an earlier switch's: before the line)
		c.set(f)
		start := f.clock
		name, sent, err := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
		if err != nil || name != "Haiku 5.5" || sent != lineMaybe || f.session != "Haiku 5.5" {
			t.Errorf("%s: %q %v %v (its model %q)", c.what, name, sent, err, f.session)
		}
		if len(f.typed) != 1 || f.typed[0] != "/model haiku" {
			t.Errorf("%s: typed %q", c.what, f.typed)
		}
		if want := f.history; (len(f.pressed) == 1 && f.pressed[0] == "yes") != want || !want && len(f.pressed) != 0 {
			t.Errorf("%s: pressed %q", c.what, f.pressed)
		}
		if spent := f.clock.Sub(start); spent > lineLogFirst+lineLogAfter+time.Second {
			t.Errorf("%s: waited %v", c.what, spent)
		}
	}
}

func TestModelLineFlowNothingTyped(t *testing.T) {
	for _, c := range []struct {
		what string
		set  func(f *fakeLine)
		want error
	}{
		{"working", func(f *fakeLine) { f.busy = true }, errBusyAgent},
		{"a draft", func(f *fakeLine) { f.draft = "half a thought" }, errDraft},
		{"a picker left open", func(f *fakeLine) { f.open = true }, errScreen},
		{"a question left open", func(f *fakeLine) { f.asking = "Sonnet 5.5" }, errScreen},
	} {
		f := newFakeLine()
		c.set(f)
		_, sent, err := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
		if !errors.Is(err, c.want) || sent != lineNotSent || len(f.typed) != 0 || len(f.pressed) != 0 {
			t.Errorf("%s: %v %v, typed %q pressed %q", c.what, sent, err, f.typed, f.pressed)
		}
	}
	// (Refused by the gate before its paste: nothing reached it.)
	for _, status := range []int{3, 7, 9, 10, 11, 12, 13, 14, 15, 17} {
		f := newFakeLine()
		f.typeErr = &sshx.ExitError{Status: status}
		if _, sent, err := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5"); sent != lineNotSent || err == nil {
			t.Errorf("exit %d: %v %v", status, sent, err)
		}
	}
	f := newFakeLine()
	f.typeErr = sshx.ErrBusy
	if _, sent, _ := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5"); sent != lineNotSent {
		t.Error("the host busy: taken for sent")
	}
}

// What typing gave back does not say nothing was typed: the default is to
// be watched, whether or not the line reached it.
func TestModelLineFlowMaybeTyped(t *testing.T) {
	for _, err := range []error{&sshx.ExitError{Status: 5}, &sshx.ExitError{Status: 6}, &sshx.ExitError{Status: 16}, errors.New("the connection was lost"), errors.New("context deadline exceeded")} {
		for _, lands := range []bool{false, true} {
			f := newFakeLine()
			f.typeErr, f.lands = err, lands
			_, sent, got := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
			if sent != lineMaybe || got == nil {
				t.Errorf("%v (landed %v): %v %v", err, lands, sent, got)
			}
		}
	}
}

func TestModelLineFlowElsewhere(t *testing.T) {
	f := newFakeLine()
	f.names["haiku"] = "Haiku 4.5" // (its word is another model's by now)
	name, sent, err := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
	if !errors.Is(err, errElsewhere) || name != "Haiku 4.5" || sent != lineMaybe {
		t.Fatalf("%q %v %v", name, sent, err)
	}
}

func TestModelLineFlowUnsure(t *testing.T) {
	for _, c := range []struct {
		what string
		set  func(f *fakeLine)
	}{
		{"its log silent", func(f *fakeLine) { f.silent = true }},
		{"only another session's answer", func(f *fakeLine) {
			f.silent, f.hidden = true, lineRecords("sid-2", "z1", "haiku", "Haiku 5.5")
		}},
		{"its record too late", func(f *fakeLine) { f.lateBy = time.Minute }},
		{"given up", func(f *fakeLine) { f.givenUp, f.lateBy = true, 300*time.Millisecond }},
	} {
		f := newFakeLine()
		c.set(f)
		start := f.clock
		_, sent, err := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
		if !errors.Is(err, errUnsure) || sent != lineMaybe || len(f.pressed) != 0 {
			t.Errorf("%s: %v %v, pressed %q", c.what, sent, err, f.pressed)
		}
		if spent := f.clock.Sub(start); spent > lineLogFirst+lineLogAfter+time.Second {
			t.Errorf("%s: waited %v", c.what, spent)
		}
	}
}

// Its question: Enter only for the model asked for and only on the screen
// read; else left with Escape, and never left standing to be answered later.
func TestModelLineFlowQuestion(t *testing.T) {
	// For another model: Escape, nothing taken.
	f := newFakeLine()
	f.history, f.asksFor = true, "Sonnet 5.5"
	_, sent, err := modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
	if !errors.Is(err, errNotTaken) || sent != lineMaybe || strings.Join(f.pressed, ",") != "esc" || f.session != "" {
		t.Errorf("for another model: %v %v, pressed %q, its model %q", sent, err, f.pressed, f.session)
	}
	// The screen moved before Enter: not pressed; the question, still there
	// on a new read, left.
	f = newFakeLine()
	f.history, f.moves = true, 1
	_, _, err = modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
	if !errors.Is(err, errNotTaken) || strings.Join(f.pressed, ",") != "esc" || f.asking != "" || f.session != "" {
		t.Errorf("the screen moved: %v, pressed %q, asking %q", err, f.pressed, f.asking)
	}
	// It keeps moving: nothing pressed at all, and that is said.
	f = newFakeLine()
	f.history, f.moves = true, 5
	_, sent, err = modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
	if !errors.Is(err, errUnsure) || sent != lineMaybe || len(f.pressed) != 0 {
		t.Errorf("the screen keeps moving: %v %v, pressed %q", sent, err, f.pressed)
	}
	// Enter did nothing: the question is not left standing.
	f = newFakeLine()
	f.history, f.stuck = true, true
	_, _, err = modelLineFlow(f.io(), "sid-1", "haiku", "Haiku 5.5")
	if !errors.Is(err, errNotTaken) || strings.Join(f.pressed, ",") != "yes,esc" || f.asking != "" {
		t.Errorf("Enter did nothing: %v, pressed %q, asking %q", err, f.pressed, f.asking)
	}
}

// fakeSettings: an account's settings, changed under a write now and then.
type fakeSettings struct {
	doc      string
	none     bool
	readErr  error
	writeErr error
	meddle   []string // before each write, the settings become this ("": left)
	writes   int
}

func (f *fakeSettings) io() settleIO {
	return settleIO{
		read: func() ([]byte, bool, error) { return []byte(f.doc), f.none, f.readErr },
		write: func(was, now []byte) (bool, error) {
			if len(f.meddle) > 0 {
				if m := f.meddle[0]; m != "" {
					f.doc = m
				}
				f.meddle = f.meddle[1:]
			}
			if string(was) != f.doc {
				return true, nil
			}
			if f.writeErr != nil {
				return false, f.writeErr
			}
			f.doc = string(now)
			f.writes++
			return false, nil
		},
	}
}

func TestSettleDefault(t *testing.T) {
	raw := func(s string) json.RawMessage {
		if s == "" {
			return nil
		}
		return json.RawMessage(s)
	}
	for _, c := range []struct {
		what       string
		f          fakeSettings
		was, wrote string // ("" was: there was none)
		want       settled
		model      string // the settings' model after ("": none)
		writes     int
	}{
		{"as before", fakeSettings{doc: `{"model": "opus", "x": 1}`}, `"opus"`, `"haiku"`, settleSame, `"opus"`, 0},
		{"none before, none now", fakeSettings{doc: `{"x": 1}`}, "", `"haiku"`, settleSame, "", 0},
		{"no settings before, none now", fakeSettings{none: true}, "", `"haiku"`, settleSame, "", 0},
		{"the line's word: put back", fakeSettings{doc: `{"model": "haiku", "x": 1}`}, `"opus"`, `"haiku"`, settleBack, `"opus"`, 1},
		{"the line's word, none before: taken out", fakeSettings{doc: `{"x": 1, "model": "haiku"}`}, "", `"haiku"`, settleBack, "", 1},
		{"the same word as before", fakeSettings{doc: `{"model": "haiku"}`}, `"haiku"`, `"haiku"`, settleSame, `"haiku"`, 0},
		{"someone else's", fakeSettings{doc: `{"model": "sonnet"}`}, `"opus"`, `"haiku"`, settleForeign, `"sonnet"`, 0},
		{"taken out by someone else", fakeSettings{doc: `{"x": 1}`}, `"opus"`, `"haiku"`, settleForeign, "", 0},
		{"the settings gone", fakeSettings{none: true}, `"opus"`, `"haiku"`, settleForeign, "", 0},
		{"what the line wrote not known", fakeSettings{doc: `{"model": "haiku"}`}, `"opus"`, "", settleForeign, `"haiku"`, 0},
		{"changed elsewhere under the write, still the line's word", fakeSettings{doc: `{"model": "haiku"}`, meddle: []string{`{"model": "haiku", "y": 2}`}}, `"opus"`, `"haiku"`, settleBack, `"opus"`, 1},
		{"someone else's under the write", fakeSettings{doc: `{"model": "haiku"}`, meddle: []string{`{"model": "fable"}`}}, `"opus"`, `"haiku"`, settleForeign, `"fable"`, 0},
		{"put back by another under the write", fakeSettings{doc: `{"model": "haiku"}`, meddle: []string{`{"model": "opus"}`}}, `"opus"`, `"haiku"`, settleSame, `"opus"`, 0},
		{"changing each time", fakeSettings{doc: `{"model": "haiku"}`, meddle: []string{`{"model": "haiku", "a": 1}`, `{"model": "haiku", "a": 2}`, `{"model": "haiku", "a": 3}`}}, `"opus"`, `"haiku"`, settleFailed, `"haiku"`, 0},
		{"not read", fakeSettings{doc: `{"model": "haiku"}`, readErr: errors.New("no answer")}, `"opus"`, `"haiku"`, settleFailed, `"haiku"`, 0},
		{"not written", fakeSettings{doc: `{"model": "haiku"}`, writeErr: errors.New("no answer")}, `"opus"`, `"haiku"`, settleFailed, `"haiku"`, 0},
		{"not JSON", fakeSettings{doc: `{"model": `}, `"opus"`, `"haiku"`, settleFailed, "?", 0},
	} {
		f := c.f
		got, err := settleDefault(f.io(), c.was != "", raw(c.was), raw(c.wrote))
		if got != c.want || (got == settleFailed) != (err != nil) {
			t.Errorf("%s: %v (%v), want %v", c.what, got, err, c.want)
		}
		if f.writes != c.writes {
			t.Errorf("%s: %d writes", c.what, f.writes)
		}
		if c.model == "?" {
			continue
		}
		cur, has, err := settingsModel([]byte(f.doc), f.none)
		if err != nil || has != (c.model != "") || has && !jsonSame(cur, raw(c.model)) {
			t.Errorf("%s: the model after: %s (%v, %v)", c.what, cur, has, err)
		}
		// (All else in the settings as it was.)
		if got == settleBack && !strings.Contains(f.doc, `"x": 1`) && strings.Contains(c.f.doc, `"x": 1`) {
			t.Errorf("%s: another setting lost: %s", c.what, f.doc)
		}
	}
}

func TestLineNotTyped(t *testing.T) {
	for status := 0; status < 20; status++ {
		maybe := status == 5 || status == 6 || status == 16
		known := status == 3 || status == 4 || (status >= 7 && status <= 15) || status == 17
		got := lineNotTyped(&sshx.ExitError{Status: status})
		if maybe && got || known && !got {
			t.Errorf("exit %d: nothing typed %v", status, got)
		}
		if !maybe && !known && got {
			t.Errorf("exit %d (not one of the gate's): taken for nothing typed", status)
		}
	}
	if lineNotTyped(errors.New("lost")) || lineNotTyped(fmt.Errorf("wrapped: %w", &sshx.ExitError{Status: 6})) {
		t.Error("an error that says nothing: taken for nothing typed")
	}
}
