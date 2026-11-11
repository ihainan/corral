package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

// fakeLine stands in for a Claude Code taking "/model NAME" on one line,
// as the real one does (out-e10, out-e11, out-e14, and the logs of
// 2026-11-10): any name is taken, its log says so a moment later (the
// command's record, then its answer), with turns behind it, it asks first,
// and its question left, its log says it kept its model. It is no more
// helpful than the real one and its host: reads fail, records come late
// and in pieces, other sessions write to the same log, the screen becomes
// another under a key.
type fakeLine struct {
	names   map[string]string // what a word typed is the name of
	history bool              // it asks "Switch model?" first
	asksFor string            // (its question names this, not what was typed)
	silent  bool              // its log never says
	lateBy  time.Duration     // its records show this long after it took the model
	drip    int               // bytes of them shown by each read (0: all at once)
	failing int               // reads of the log that fail first
	typeErr error             // what typing gives back…
	lands   bool              // …the line having reached it all the same
	swapTo  string            // before the next press, its question becomes one for this model (someone's own)
	deaf    bool              // presses are refused, whatever the screen (no sha256sum on the host)
	narrow  bool              // its question as a narrow pane shows it
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
	case f.asking != "" && f.narrow:
		// (As a pane of 39 columns shows it: narrow-plain.txt, 2026-11-10.)
		b.WriteString("▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔\n   Switch model?\n   Your next response will be slower\n   and use more tokens\n\n   This conversation is cached for\n   the current model. Switching to\n   " +
			f.asking + " means the full history\n   gets re-read.\n\n   ❯ 1. Yes, switch to " + f.asking + "\n     2. No, go back\n")
	case f.asking != "":
		// (As a pane of 120 columns shows it.)
		b.WriteString("▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔\n   Switch model?\n   Your next response will be slower and use more tokens\n\n   This conversation is cached for the current model. Switching to " +
			f.asking + " means the full history gets re-read.\n\n   ❯ 1. Yes, switch to " + f.asking + "\n     2. No, go back\n")
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

func lineSystemRecord(sid, uuid, parent, content string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "system", "subtype": "local_command", "sessionId": sid, "uuid": uuid, "parentUuid": parent, "content": content})
	return append(b, '\n')
}

func lineCommand(word string) string {
	return "<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args>" + word + "</command-args>"
}

// lineRecords: what its log gains when the line is taken.
func lineRecords(sid, uuid, word, name string) []byte {
	out := "<local-command-stdout>Set model to `" + name + "` and saved as your default for new sessions</local-command-stdout>"
	return append(lineRecord(sid, uuid, "p-"+uuid, lineCommand(word)), lineRecord(sid, "a-"+uuid, uuid, out)...)
}

// lineKeptRecords: what it gains when its question is left.
func lineKeptRecords(sid, uuid, word, had string) []byte {
	out := "<local-command-stdout>Kept model as `" + had + "`</local-command-stdout>"
	return append(lineSystemRecord(sid, uuid, "p-"+uuid, lineCommand(word)), lineSystemRecord(sid, "a-"+uuid, uuid, out)...)
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

func (f *fakeLine) logFrom(off int64) ([]byte, error) {
	if f.failing > 0 {
		f.failing--
		return nil, errors.New("the host did not answer")
	}
	if len(f.hidden) > 0 && !f.clock.Before(f.showAt) {
		n := len(f.hidden)
		if f.drip > 0 {
			n = min(n, f.drip)
		}
		f.log, f.hidden = append(f.log, f.hidden[:n]...), f.hidden[n:]
	}
	return f.log[min(int(off), len(f.log)):], nil
}

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
			if f.swapTo != "" {
				// (Someone left the line's question and opened their own.)
				if !f.silent {
					f.hidden = append(f.hidden, lineKeptRecords(f.sid, fmt.Sprintf("c%d", len(f.typed)), f.word, "Opus 5.5")...)
				}
				f.asking, f.word, f.swapTo = f.swapTo, "theirs", ""
				f.draws++
			}
			if f.deaf {
				return &sshx.ExitError{Status: 11}
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
				if !f.silent {
					f.hidden = append(f.hidden, lineKeptRecords(f.sid, fmt.Sprintf("c%d", len(f.typed)), f.word, "Opus 5.5")...)
				}
			}
			f.draws++
			return nil
		},
		logFrom: f.logFrom,
		now:     func() time.Time { return f.clock },
		sleep: func(d time.Duration) error {
			if f.givenUp {
				return errors.New("given up")
			}
			f.clock = f.clock.Add(d)
			return nil
		},
	}
}

// run types the line for Haiku into f, as convoModelLine does.
func (f *fakeLine) run() (log *lineLog, off int64, name string, sent lineSent, err error) {
	log, off = &lineLog{sid: f.sid, arg: "haiku"}, int64(len(f.log))
	name, sent, err = modelLineFlow(f.io(), log, &off, "Haiku 5.5")
	return
}

func TestLineModelArg(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := lineListKey("s1", 42, 7)
	rows := []modelOpt{
		{Label: "Default (recommended)", Desc: "Opus 5.5 · Most capable"},
		{Label: "Haiku", Desc: "Haiku 5.5 · Fastest for quick answers"},
		{Label: "Sonnet", Desc: "Sonnet 5.5 · Best for everyday tasks"},
		{Label: "Opus", Desc: "Something else · no version"},
		{Label: "Fable", Desc: "Opus 5.5 · another's name"},
		{Label: "Opus 5.5 (1M context)", Desc: "Opus 5.5 · long"},
	}
	noteOffered(key, rows, now)
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
	// (Its rows are kept as given, for the page to be told at once; the one
	// it is on, moved by a switch from here; forgotten when told to.)
	noteOffered(key, rows, now)
	if opts, ok := offered(key, now.Add(time.Hour)); !ok || len(opts) != len(rows) {
		t.Errorf("its list an hour on: %v %v", opts, ok)
	}
	offeredNow(key, "Haiku")
	opts, _ := offered(key, now.Add(time.Hour))
	for _, o := range opts {
		if o.Current != (o.Label == "Haiku") {
			t.Errorf("after a switch to Haiku: %+v", o)
		}
	}
	if _, ok := offered(key, now.Add(lineListFor+time.Second)); ok {
		t.Error("its list kept too long")
	}
	kin := lineKinKey("acct", "relay.example.com", "key")
	if _, ok := offered(kin, now); ok || kin == lineKinKey("acct", "", "key") || kin == lineKinKey("acct2", "relay.example.com", "key") {
		t.Error("a list of others' taken for kin's")
	}
	// A process shown its kin's list may be typed to on one line only for
	// what its own picker was since seen to offer.
	noteOffered(kin, rows, now)
	mine := lineListKey("acct", 99, 1)
	if _, _, ok := lineModelArg(mine, "Haiku", now); ok {
		t.Error("its kin's list taken for its own")
	}
	confirmOffered(mine, kin, modelOpt{Label: "Haiku", Desc: "Haiku 5.5 · Fastest for quick answers"}, now)
	if arg, name, ok := lineModelArg(mine, "Haiku", now); !ok || arg != "haiku" || name != "Haiku 5.5" {
		t.Errorf("Haiku, taken through its own picker: %q %q %v", arg, name, ok)
	}
	if _, _, ok := lineModelArg(mine, "Sonnet", now); ok {
		t.Error("Sonnet, only in its kin's list: taken for one of its own")
	}
	if got, ok := offered(mine, now); !ok || len(got) != len(rows) {
		t.Errorf("its list after: %v", got)
	} else {
		for _, o := range got {
			if o.Current != (o.Label == "Haiku") {
				t.Errorf("the one it is on: %+v", o)
			}
		}
	}
	forgetOffered(kin, mine)
	if _, ok := offered(kin, now); ok {
		t.Error("a list forgotten still kept")
	}
	if _, _, ok := lineModelArg(key, "Sonnet", now.Add(time.Minute)); ok {
		t.Error("a list grown old was not let go")
	}
}

func TestLineLogFeed(t *testing.T) {
	ours := lineRecords("sid-1", "c1", "haiku", "Haiku 5.5")
	// feedAll: the log read as it grows, grow bytes more of it each time.
	feedAll := func(l *lineLog, data []byte, grow int) {
		off := 0
		for have := min(grow, len(data)); ; have = min(have+grow, len(data)) {
			off += l.feed(data[off:have])
			if have == len(data) {
				return
			}
		}
	}
	for _, grow := range []int{1 << 20, 50, 7, 1} {
		l := lineLog{sid: "sid-1", arg: "haiku"}
		if feedAll(&l, ours, grow); l.end != lineRan || l.name != "Haiku 5.5" {
			t.Errorf("growing by %d: %v %q", grow, l.end, l.name)
		}
	}
	l := lineLog{sid: "sid-1", arg: "haiku"}
	if feedAll(&l, lineKeptRecords("sid-1", "c1", "haiku", "Opus 5.5"), 1<<20); l.end != lineKept {
		t.Errorf("its question left: %v", l.end)
	}
	answer := "<local-command-stdout>Set model to `Haiku 5.5` and saved as your default for new sessions</local-command-stdout>"
	not := map[string][]byte{
		"another session's":         lineRecords("sid-2", "c1", "haiku", "Haiku 5.5"),
		"another session's, left":   lineKeptRecords("sid-2", "c1", "haiku", "Opus 5.5"),
		"an answer with no command": lineRecord("sid-1", "a1", "c1", answer),
		"another word's command":    lineRecords("sid-1", "c1", "sonnet", "Haiku 5.5"),
		"the picker's, left":        lineKeptRecords("sid-1", "c1", "", "Opus 5.5"),
		"the answer of another command": append(lineRecord("sid-1", "c1", "p", lineCommand("haiku")),
			lineRecord("sid-1", "a1", "c0", answer)...),
		"quoted in a message": append(lineRecord("sid-1", "c1", "p", "look: "+lineCommand("haiku")),
			lineRecord("sid-1", "a1", "c1", "it said "+answer)...),
		"an assistant's": []byte(`{"type":"assistant","sessionId":"sid-1","uuid":"c1","message":{"content":"<command-name>/model</command-name><command-args>haiku</command-args>"}}` + "\n" +
			`{"type":"assistant","sessionId":"sid-1","uuid":"a1","parentUuid":"c1","message":{"content":"` + strings.ReplaceAll(answer, "`", "\\u0060") + `"}}` + "\n"),
		"a system record of another kind": []byte(`{"type":"system","subtype":"turn_duration","sessionId":"sid-1","uuid":"c1","content":"<command-name>/model</command-name><command-args>haiku</command-args>"}` + "\n" +
			`{"type":"system","subtype":"turn_duration","sessionId":"sid-1","uuid":"a1","parentUuid":"c1","content":"<local-command-stdout>Kept model as x</local-command-stdout>"}` + "\n"),
		"not whole": ours[:len(ours)-1],
	}
	for what, data := range not {
		l := lineLog{sid: "sid-1", arg: "haiku"}
		if feedAll(&l, data, 1<<20); l.end != lineOpen {
			t.Errorf("%s: taken for its end (%v %q)", what, l.end, l.name)
		}
	}
	// (The command's record alone: known to have been recorded, not ended.)
	l = lineLog{sid: "sid-1", arg: "haiku"}
	if feedAll(&l, lineRecord("sid-1", "c1", "p", lineCommand("haiku")), 1<<20); l.end != lineOpen || len(l.cmds) != 1 {
		t.Errorf("the command alone: %v, %d commands", l.end, len(l.cmds))
	}
	// (A record too long to wait for is passed over, what follows it read.)
	long := append(lineRecord("sid-1", "x", "", strings.Repeat("y", 3*lineLogSkip)), ours...)
	l = lineLog{sid: "sid-1", arg: "haiku"}
	if feedAll(&l, long, lineLogSkip); l.end != lineRan || l.name != "Haiku 5.5" {
		t.Errorf("after a long record: %v %q", l.end, l.name)
	}
}

func TestModelLineFlowSwitches(t *testing.T) {
	for _, c := range []struct {
		what string
		set  func(f *fakeLine)
	}{
		{"at once", func(f *fakeLine) {}},
		{"its record late", func(f *fakeLine) { f.lateBy = 600 * time.Millisecond }},
		{"its records coming in pieces", func(f *fakeLine) { f.drip = 40 }},
		{"reads failing first", func(f *fakeLine) { f.failing = 3 }},
		{"asked first", func(f *fakeLine) { f.history = true }},
		{"asked first, in a narrow pane", func(f *fakeLine) { f.history, f.narrow = true, true }},
		{"asked first, its record late", func(f *fakeLine) { f.history, f.lateBy = true, 1500*time.Millisecond }},
		{"asked first, its records in pieces", func(f *fakeLine) { f.history, f.drip = true, 60 }},
		{"others writing to its log", func(f *fakeLine) {
			f.hidden = append(lineRecords("sid-2", "z1", "haiku", "Opus 5.5"), lineRecords("sid-1", "z2", "sonnet", "Sonnet 5.5")...)
		}},
	} {
		f := newFakeLine()
		f.log = lineRecords("sid-1", "old", "haiku", "Haiku 4.5") // (an earlier switch's: before the line)
		c.set(f)
		start := f.clock
		_, _, name, sent, err := f.run()
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
		{"a question left open, in a narrow pane", func(f *fakeLine) { f.asking, f.narrow = "Sonnet 5.5", true }, errScreen},
	} {
		f := newFakeLine()
		c.set(f)
		_, _, _, sent, err := f.run()
		if !errors.Is(err, c.want) || sent != lineNotSent || len(f.typed) != 0 || len(f.pressed) != 0 {
			t.Errorf("%s: %v %v, typed %q pressed %q", c.what, sent, err, f.typed, f.pressed)
		}
	}
	// (Refused by the gate before its paste: nothing reached it.)
	for _, status := range []int{3, 7, 9, 10, 11, 12, 13, 14, 15, 17} {
		f := newFakeLine()
		f.typeErr = &sshx.ExitError{Status: status}
		if _, _, _, sent, err := f.run(); sent != lineNotSent || err == nil {
			t.Errorf("exit %d: %v %v", status, sent, err)
		}
	}
	f := newFakeLine()
	f.typeErr = sshx.ErrBusy
	if _, _, _, sent, _ := f.run(); sent != lineNotSent {
		t.Error("the host busy: taken for sent")
	}
}

// What typing gave back does not say nothing was typed: its end is to be
// looked for, whether or not the line reached it.
func TestModelLineFlowMaybeTyped(t *testing.T) {
	for _, err := range []error{&sshx.ExitError{Status: 5}, &sshx.ExitError{Status: 6}, &sshx.ExitError{Status: 16}, errors.New("the connection was lost"), errors.New("context deadline exceeded")} {
		for _, lands := range []bool{false, true} {
			f := newFakeLine()
			f.typeErr, f.lands = err, lands
			_, _, _, sent, got := f.run()
			if sent != lineMaybe || got == nil {
				t.Errorf("%v (landed %v): %v %v", err, lands, sent, got)
			}
		}
	}
}

func TestModelLineFlowElsewhere(t *testing.T) {
	f := newFakeLine()
	f.names["haiku"] = "Haiku 4.5" // (its word is another model's by now)
	_, _, name, sent, err := f.run()
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
		log, _, _, sent, err := f.run()
		if !errors.Is(err, errUnsure) || sent != lineMaybe || len(f.pressed) != 0 || log.end != lineOpen {
			t.Errorf("%s: %v %v, pressed %q, end %v", c.what, sent, err, f.pressed, log.end)
		}
		if spent := f.clock.Sub(start); spent > lineLogFirst+lineLogAfter+time.Second {
			t.Errorf("%s: waited %v", c.what, spent)
		}
	}
}

// Its question: Enter only for the model asked for and only on the screen
// read; left with Escape only where it is this line's; never another's.
func TestModelLineFlowQuestion(t *testing.T) {
	// A question for another model where ours was looked for (someone left
	// ours and opened their own before the first look, or the line's word
	// is another model's by now): not known to be this line's — neither
	// answered nor left.
	f := newFakeLine()
	f.history, f.asksFor = true, "Sonnet 5.5"
	_, _, _, sent, err := f.run()
	if !errors.Is(err, errUnsure) || sent != lineMaybe || len(f.pressed) != 0 || f.asking != "Sonnet 5.5" || f.session != "" {
		t.Errorf("for another model: %v %v, pressed %q, asking %q, its model %q", sent, err, f.pressed, f.asking, f.session)
	}
	// The same, ours left before it, its log saying so: not taken.
	f = newFakeLine()
	f.history, f.asksFor = true, "Sonnet 5.5"
	f.hidden = lineKeptRecords("sid-1", "c1", "haiku", "Opus 5.5")
	if _, _, _, _, err = f.run(); !errors.Is(err, errNotTaken) || len(f.pressed) != 0 || f.asking != "Sonnet 5.5" {
		t.Errorf("another's question, ours left before the first look: %v, pressed %q, asking %q", err, f.pressed, f.asking)
	}
	// Someone left it and opened their own, for another model, before
	// Enter: Enter not pressed, and theirs neither answered nor left. Its
	// log says ours was left.
	for _, narrow := range []bool{false, true} {
		f = newFakeLine()
		f.history, f.swapTo, f.narrow = true, "Sonnet 5.5", narrow
		log, _, _, _, err := f.run()
		if !errors.Is(err, errNotTaken) || len(f.pressed) != 0 || f.asking != "Sonnet 5.5" || f.session != "" || log.end != lineKept {
			t.Errorf("another's question in its place (narrow %v): %v, pressed %q, asking %q, end %v", narrow, err, f.pressed, f.asking, log.end)
		}
	}
	// The same, its log silent: not known, theirs still untouched.
	f = newFakeLine()
	f.history, f.swapTo, f.silent = true, "Sonnet 5.5", true
	if _, _, _, _, err = f.run(); !errors.Is(err, errUnsure) || len(f.pressed) != 0 || f.asking != "Sonnet 5.5" {
		t.Errorf("another's question, the log silent: %v, pressed %q, asking %q", err, f.pressed, f.asking)
	}
	// No key can be pressed at all (the host cannot name its screen): the
	// question stands, and that is said — for the caller to keep watch.
	f = newFakeLine()
	f.history, f.deaf = true, true
	log, _, _, sent, err := f.run()
	if !errors.Is(err, errUnsure) || sent != lineMaybe || len(f.pressed) != 0 || f.asking != "Haiku 5.5" || log.end != lineOpen {
		t.Errorf("no key can be pressed: %v %v, pressed %q, asking %q", sent, err, f.pressed, f.asking)
	}
	// Enter did nothing: the question is not left standing.
	f = newFakeLine()
	f.history, f.stuck = true, true
	_, _, _, _, err = f.run()
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
	mtime    time.Time // when they were last written, by the host's clock
	stampErr error
}

func (f *fakeSettings) io() settleIO {
	return settleIO{
		read:  func() ([]byte, bool, error) { return []byte(f.doc), f.none, f.readErr },
		stamp: func() (time.Time, error) { return f.mtime, f.stampErr },
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

func (f *fakeSettings) model() string {
	cur, has, err := settingsModel([]byte(f.doc), f.none)
	if err != nil {
		return "?"
	}
	if !has {
		return ""
	}
	return string(cur)
}

func rawJSON(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func TestSettleDefault(t *testing.T) {
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
		got, err := settleDefault(f.io(), c.was != "", rawJSON(c.was), rawJSON(c.wrote))
		if got != c.want || (got == settleFailed) != (err != nil) {
			t.Errorf("%s: %v (%v), want %v", c.what, got, err, c.want)
		}
		if f.writes != c.writes {
			t.Errorf("%s: %d writes", c.what, f.writes)
		}
		if m := f.model(); m != "?" && (c.model == "") != (m == "") || m != "?" && c.model != "" && !jsonSame(rawJSON(m), rawJSON(c.model)) {
			t.Errorf("%s: the model after: %s", c.what, m)
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

// fakeWhere: where a line was typed, as lineProbe looks at it.
type fakeWhere struct {
	log    []byte
	dead   bool // its process is gone
	noProc bool // (the host cannot tell)
	rest   bool // its pane shows it at rest, the box empty
	moved  bool // its process holds another conversation by now
	logErr error
	onWait func()           // (what comes about while a look waits a moment)
	clock  func() time.Time // the host's clock (nil: it does not say)
}

// stamped: records, each said to be written at at.
func stamped(rows []byte, at time.Time) []byte {
	var out []byte
	for _, row := range bytes.Split(bytes.TrimSuffix(rows, []byte("\n")), []byte("\n")) {
		out = append(append(append(out, []byte(`{"timestamp":"`+at.UTC().Format(time.RFC3339Nano)+`",`)...), row[1:]...), '\n')
	}
	return out
}

func (f *fakeWhere) probe() lineProbe {
	return lineProbe{
		logFrom: func(off int64) ([]byte, error) {
			if f.logErr != nil {
				return nil, f.logErr
			}
			return f.log[min(int(off), len(f.log)):], nil
		},
		alive: func() (bool, bool) { return !f.dead, !f.noProc },
		wait: func() {
			if f.onWait != nil {
				f.onWait()
			}
		},
		hostNow: func() (time.Time, bool) {
			if f.clock == nil {
				return time.Time{}, false
			}
			return f.clock(), true
		},
		moved:  func() bool { return f.moved },
		atRest: func() bool { return f.rest },
	}
}

// What is kept of a line is let go only once the line is known to be over
// and the default settled — never for having been looked at so many times.
func TestLineConclude(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	before := lineRecords("sid-1", "old", "haiku", "Haiku 5.5") // (in the log before the line: not its end)
	watch := func(was string) *lineWatch {
		return &lineWatch{had: was != "", was: rawJSON(was), wrote: rawJSON(`"haiku"`), log: &lineLog{sid: "sid-1", arg: "haiku"}, off: int64(len(before)), typedAt: t0}
	}
	ran := lineRecords("sid-1", "c1", "haiku", "Haiku 5.5")
	kept := lineKeptRecords("sid-1", "c1", "haiku", "Opus 5.5")
	cmdOnly := lineRecord("sid-1", "c1", "p", lineCommand("haiku"))
	for _, c := range []struct {
		what   string
		where  fakeWhere
		doc    string
		was    string
		after  time.Duration
		end    lineEnd
		res    settled
		final  bool
		model  string
		rebase bool
	}{
		{"its question standing", fakeWhere{}, `{"model": "opus"}`, `"opus"`, 5 * time.Second, lineOpen, settleSame, false, `"opus"`, false},
		{"its question standing, hours on", fakeWhere{}, `{"model": "opus"}`, `"opus"`, 10 * time.Second, lineOpen, settleSame, false, `"opus"`, false},
		{"its question standing, the host cannot tell of its process", fakeWhere{noProc: true, dead: true}, `{"model": "opus"}`, `"opus"`, 10 * time.Second, lineOpen, settleSame, false, `"opus"`, false},
		{"the line still in the box (not at rest), no record of it", fakeWhere{}, `{}`, "", 10 * time.Second, lineOpen, settleSame, false, "", false},
		{"taken, its word there", fakeWhere{log: ran}, `{"model": "haiku"}`, `"opus"`, time.Second, lineRan, settleBack, true, `"opus"`, false},
		{"taken, its word there, none before", fakeWhere{log: ran}, `{"a": 1, "model": "haiku"}`, "", time.Second, lineRan, settleBack, true, "", false},
		{"taken, its word not written yet", fakeWhere{log: ran}, `{"model": "opus"}`, `"opus"`, time.Second, lineRan, settleSame, false, `"opus"`, false},
		{"taken, its word never came", fakeWhere{log: ran}, `{"model": "opus"}`, `"opus"`, lineSettleAge, lineRan, settleSame, true, `"opus"`, false},
		{"taken, the same word as before", fakeWhere{log: ran}, `{"model": "haiku"}`, `"haiku"`, time.Second, lineRan, settleSame, true, `"haiku"`, false},
		{"taken, someone else's there since", fakeWhere{log: ran}, `{"model": "fable"}`, `"opus"`, time.Second, lineRan, settleForeign, true, `"fable"`, false},
		{"its question left", fakeWhere{log: kept}, `{"model": "opus"}`, `"opus"`, time.Second, lineKept, settleSame, true, `"opus"`, false},
		{"its question left, the same word set by another session since: theirs, not touched", fakeWhere{log: kept}, `{"model": "haiku"}`, `"opus"`, time.Second, lineKept, settleSame, true, `"haiku"`, false},
		{"never ran, the same word set by another session since: theirs, not touched", fakeWhere{rest: true}, `{"model": "haiku"}`, `"opus"`, 10 * time.Second, lineNever, settleSame, true, `"haiku"`, false},
		{"its process holds another conversation by now (/clear): over", fakeWhere{moved: true}, `{"model": "opus"}`, `"opus"`, time.Second, lineGone, settleSame, true, `"opus"`, false},
		{"another conversation by now, the host cannot tell of its process: not known", fakeWhere{moved: true, noProc: true}, `{"model": "opus"}`, `"opus"`, 10 * time.Second, lineOpen, settleSame, false, `"opus"`, false},
		{"its process gone", fakeWhere{dead: true}, `{"model": "opus"}`, `"opus"`, time.Second, lineGone, settleSame, true, `"opus"`, false},
		{"its process gone, the same word there (it wrote nothing: another session's)", fakeWhere{dead: true}, `{"model": "haiku"}`, `"opus"`, time.Second, lineGone, settleSame, true, `"haiku"`, false},
		{"its process gone, its log not read, its word there: put back", fakeWhere{dead: true, logErr: errors.New("denied")}, `{"model": "haiku"}`, `"opus"`, time.Second, lineGone, settleBack, true, `"opus"`, false},
		{"its process gone, its log not read, the default as before", fakeWhere{dead: true, logErr: errors.New("denied")}, `{"model": "opus"}`, `"opus"`, time.Second, lineGone, settleSame, true, `"opus"`, false},
		{"its log not read, at rest, its word there: not known, kept, not touched", fakeWhere{rest: true, logErr: errors.New("denied")}, `{"model": "haiku"}`, `"opus"`, time.Hour, lineOpen, settleSame, false, `"haiku"`, false},
		{"its log not read, another conversation by now: kept", fakeWhere{moved: true, logErr: errors.New("denied")}, `{"model": "haiku"}`, `"opus"`, time.Hour, lineOpen, settleSame, false, `"haiku"`, false},
		{"another conversation by now, the same word set by another session: theirs, not touched", fakeWhere{moved: true}, `{"model": "haiku"}`, `"opus"`, 10 * time.Second, lineGone, settleSame, true, `"haiku"`, false},
		{"never ran: at rest, no record of it", fakeWhere{rest: true}, `{"model": "opus"}`, `"opus"`, lineNeverAge, lineNever, settleSame, true, `"opus"`, false},
		{"at rest a moment after it was typed: too soon to say", fakeWhere{rest: true}, `{"model": "opus"}`, `"opus"`, time.Second, lineOpen, settleSame, false, `"opus"`, false},
		{"at rest, its command recorded, no answer", fakeWhere{rest: true, log: cmdOnly}, `{"model": "opus"}`, `"opus"`, 10 * time.Second, lineOpen, settleSame, false, `"opus"`, false},
		{"open, its word there (another session's: the line wrote nothing): not touched, theirs to keep", fakeWhere{}, `{"model": "haiku"}`, `"opus"`, time.Second, lineOpen, settleForeign, false, `"haiku"`, true},
		{"open, the settings not read: kept, no failure of this line's", fakeWhere{}, `{"model": `, `"opus"`, 10 * time.Second, lineOpen, settleSame, false, "?", false},
		{"open, someone else's there", fakeWhere{}, `{"model": "fable"}`, `"opus"`, 10 * time.Second, lineOpen, settleForeign, false, `"fable"`, true},
		{"open, taken out by someone else", fakeWhere{}, `{}`, `"opus"`, 10 * time.Second, lineOpen, settleForeign, false, "", true},
		{"open long after it was typed: let go, whatever becomes of it is the person's own", fakeWhere{}, `{"model": "opus"}`, `"opus"`, lineOwn + lineOwnGrace + time.Second, lineOpen, settleSame, true, `"opus"`, false},
		{"open long after, another's default there: let go, nothing noted", fakeWhere{}, `{"model": "fable"}`, `"opus"`, time.Hour, lineOpen, settleSame, true, `"fable"`, false},
		{"open, just past the time a line is ours: kept a little longer", fakeWhere{}, `{"model": "opus"}`, `"opus"`, lineOwn + time.Second, lineOpen, settleSame, false, `"opus"`, false},
		{"taken, found long after, when not said: not taken for ours, not touched", fakeWhere{log: ran}, `{"model": "haiku"}`, `"opus"`, time.Hour, lineRan, settleSame, true, `"haiku"`, false},
		{"taken, the settings not read", fakeWhere{log: ran}, `{"model": `, `"opus"`, 10 * time.Second, lineRan, settleFailed, false, "?", false},
	} {
		f := c.where
		f.log = append(append([]byte{}, before...), f.log...)
		st := fakeSettings{doc: c.doc}
		w := watch(c.was)
		end, res, final, rebased, err := lineConclude(f.probe(), st.io(), w, t0.Add(c.after))
		if end != c.end || res != c.res || final != c.final || rebased != c.rebase || (res == settleFailed) != (err != nil) {
			t.Errorf("%s: end %v settled %v final %v rebased %v (%v); want %v %v %v %v", c.what, end, res, final, rebased, err, c.end, c.res, c.final, c.rebase)
		}
		if m := st.model(); m != c.model && !(m != "" && m != "?" && c.model != "" && jsonSame(rawJSON(m), rawJSON(c.model))) {
			t.Errorf("%s: the model after: %q, want %q", c.what, m, c.model)
		}
	}
}

// A line left open is followed through looks far apart, by whoever looks:
// a gateway started again reads its log from the length kept (watchOf).
func TestLineConcludeLater(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	before := lineRecords("sid-1", "old", "haiku", "Haiku 5.5")
	rec := store.SettingsPending{Had: true, Was: []byte(`"opus"`), Wrote: []byte(`"haiku"`), SID: "sid-1", LogOff: int64(len(before)), CreatedAt: t0.Unix()}
	where := fakeWhere{log: append([]byte{}, before...)}
	st := fakeSettings{doc: `{"model": "opus"}`}
	// Its question stands open: looked at again and again, by a new watch
	// each time (as after a restart), it is never let go.
	for i, after := range []time.Duration{time.Second, 5 * time.Second, 9 * time.Second} {
		if end, _, final, _, _ := lineConclude(where.probe(), st.io(), watchOf(&rec), t0.Add(after)); final || end != lineOpen {
			t.Fatalf("look %d: let go with its question open (end %v)", i, end)
		}
	}
	// Another session sets the very word the line would write: theirs (the
	// line, open, wrote nothing) — not touched, the one to put back from
	// now on, and the line still kept.
	st.doc = `{"model": "haiku"}`
	w0 := watchOf(&rec)
	if end, _, final, rebased, _ := lineConclude(where.probe(), st.io(), w0, t0.Add(10*time.Second)); final || end != lineOpen || !rebased || string(w0.was) != `"haiku"` || st.model() != `"haiku"` || st.writes != 0 {
		t.Fatalf("the same word set by another with the line open: end %v final %v rebased %v was %s, the model %s", end, final, rebased, w0.was, st.model())
	}
	// (Its own word, written a moment before its record: the look waits
	// that moment, finds the record, and puts back.)
	lag := fakeWhere{log: append([]byte{}, before...)}
	lag.onWait = func() { lag.log = append(lag.log, lineRecords("sid-1", "c1", "haiku", "Haiku 5.5")...) }
	if end, res, final, rebased, _ := lineConclude(lag.probe(), st.io(), watchOf(&rec), t0.Add(10*time.Second)); end != lineRan || res != settleBack || !final || rebased || st.model() != `"opus"` {
		t.Fatalf("its word seen before its record: end %v settled %v final %v rebased %v, the model %s", end, res, final, rebased, st.model())
	}
	rec.Had, rec.Was = w0.had, w0.was
	// Someone sets a default of their own meanwhile: theirs is now the one
	// to put back.
	st.doc = `{"model": "fable"}`
	w := watchOf(&rec)
	if _, _, final, rebased, _ := lineConclude(where.probe(), st.io(), w, t0.Add(12*time.Second)); final || !rebased || !w.had || string(w.was) != `"fable"` {
		t.Fatalf("someone else's default with the line open: final %v rebased %v was %s", final, rebased, w.was)
	}
	rec.Had, rec.Was = w.had, w.was
	// The person answers the question at last: its word is written, its log
	// says so. The next look puts theirs back and lets go.
	where.log = append(where.log, lineRecords("sid-1", "c1", "haiku", "Haiku 5.5")...)
	st.doc = `{"model": "haiku"}`
	end, res, final, _, err := lineConclude(where.probe(), st.io(), watchOf(&rec), t0.Add(14*time.Second))
	if end != lineRan || res != settleBack || !final || err != nil || st.model() != `"fable"` {
		t.Fatalf("answered at last: end %v settled %v final %v (%v), the model %s", end, res, final, err, st.model())
	}
	// And once let go, a look more changes nothing — nor would the same
	// word, set by the person themselves, be taken for the line's again
	// (there is nothing kept to look with: this is the caller's delete).
}

// Whatever comes about around a line kept — other sessions setting the
// default, the person answering or leaving its question, quitting, a new
// conversation, its log unreadable for a while — looked at after each:
// the settings are written only where the line ran; what is kept is never
// let go while the line can still run; quitting always ends it; and the
// default, in the end, is the last one someone else set.
func TestLineConcludeSequences(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	before := lineRecords("sid-1", "old", "haiku", "Haiku 5.5")
	// ("!": not looked at after it. "later": an hour on.)
	events := []string{"same", "other", "same!", "other!", "yes", "leave", "quit", "clear", "logfail", "logback", "later", "later!"}
	type world struct {
		where    fakeWhere
		st       fakeSettings
		rec      store.SettingsPending
		asked    bool   // its question stands open (else: the line sits in the box)
		canRun   bool   // it can still be answered or sent
		ran      bool   // it ran within lineOwn of being typed: the settings were written by it, for this switch
		unseen   bool   // (another's default set, not looked at, while the line was ours)
		theirs   string // the default as others left it
		owed     bool   // the line's word is the default, written by it, not put back yet
		dropped  bool
		failed   bool // (its log was unreadable at some point)
		now      time.Time
		writesAt int
	}
	var run func(seq []string, asked bool)
	run = func(seq []string, asked bool) {
		w := &world{asked: asked, canRun: true, theirs: `"opus"`, now: t0}
		w.where.clock = func() time.Time { return w.now.Add(-7 * time.Minute) } // (the host's clock, off from this one)
		ours := func() bool { return w.now.Sub(t0) <= lineOwn }
		w.where.log = append([]byte{}, before...)
		if asked {
			w.where.log = append(w.where.log, lineRecord("sid-1", "c1", "p", lineCommand("haiku"))...)
		}
		w.st.doc = `{"model": "opus"}`
		w.rec = store.SettingsPending{Had: true, Was: []byte(`"opus"`), Wrote: []byte(`"haiku"`), SID: "sid-1", LogOff: int64(len(before)), CreatedAt: t0.Unix()}
		look := func(at string) {
			if w.dropped {
				return
			}
			lw := watchOf(&w.rec)
			_, _, final, rebased, _ := lineConclude(w.where.probe(), w.st.io(), lw, w.now)
			if w.st.writes != w.writesAt {
				if !w.ran && !(w.where.dead && w.where.logErr != nil) {
					t.Fatalf("%v (asked %v), after %s: the settings written, the line never ran", seq, asked, at)
				}
				w.writesAt, w.owed = w.st.writes, false
			}
			if rebased {
				w.rec.Had, w.rec.Was = lw.had, lw.was
			}
			if final {
				if w.canRun && w.now.Sub(t0) <= lineOwn+lineOwnGrace {
					t.Fatalf("%v (asked %v), after %s: let go while the line can still run, and would be ours", seq, asked, at)
				}
				if w.owed && !w.unseen {
					t.Fatalf("%v (asked %v), after %s: let go with the line's word left as the default", seq, asked, at)
				}
				w.dropped = true
			}
		}
		for _, e := range append(append([]string{}, seq...), "logback", "quit") {
			w.now = w.now.Add(2 * time.Second)
			unlooked := strings.HasSuffix(e, "!")
			if unlooked && e != "later!" && ours() && !w.dropped {
				w.unseen = true
			}
			switch strings.TrimSuffix(e, "!") {
			case "later":
				w.now = w.now.Add(time.Hour)
			case "same":
				w.st.doc, w.theirs, w.owed, w.st.mtime = `{"model": "haiku"}`, `"haiku"`, false, w.where.clock()
			case "other":
				w.st.doc, w.theirs, w.owed, w.st.mtime = `{"model": "fable"}`, `"fable"`, false, w.where.clock()
			case "yes": // answered yes, or the line sent from the box (asking nothing)
				if w.canRun {
					w.where.log = append(w.where.log, stamped(lineRecords("sid-1", "c1", "haiku", "Haiku 5.5"), w.where.clock())...)
					if ours() {
						w.ran, w.owed = true, w.theirs != `"haiku"`
					} else {
						w.theirs = `"haiku"` // (the person's own "/model", long after: the default they were told of)
					}
					w.st.doc, w.canRun, w.where.rest, w.st.mtime = `{"model": "haiku"}`, false, true, w.where.clock()
				}
			case "leave": // its question left, or the box cleared
				if w.canRun {
					if w.asked {
						w.where.log = append(w.where.log, lineKeptRecords("sid-1", "c1", "haiku", "Opus 5.5")...)
					}
					w.canRun, w.where.rest = false, true
				}
			case "quit":
				w.where.dead, w.canRun = true, false
			case "clear": // (not with its question open, nor the line in the box)
				if !w.canRun && !w.where.dead {
					w.where.moved, w.where.rest = true, true
				}
			case "logfail":
				w.where.logErr, w.failed = errors.New("denied"), true
			case "logback":
				w.where.logErr = nil
			}
			if !unlooked {
				look(e)
			}
		}
		if !w.dropped {
			t.Fatalf("%v (asked %v): still kept after its Claude Code was quit", seq, asked)
		}
		if m := w.st.model(); !w.failed && !w.unseen && !jsonSame(rawJSON(m), rawJSON(w.theirs)) {
			t.Fatalf("%v (asked %v): the default in the end %s, the last set by others %s", seq, asked, m, w.theirs)
		}
	}
	var each func(seq []string, n int)
	each = func(seq []string, n int) {
		if n == 0 {
			run(seq, true)
			run(seq, false)
			return
		}
		for _, e := range events {
			each(append(append([]string{}, seq...), e), n-1)
		}
	}
	for n := 0; n <= 4; n++ {
		each(nil, n)
	}
}

// A line found to have run is this switch's only where it ran within
// lineOwn of being typed — by its log's time, the host's clock set against
// this one's with both read close together.
func TestLineOurs(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		what   string
		ran    time.Duration // after it was typed
		look   time.Duration // the look begins
		read   time.Duration // reading its log takes
		ask    time.Duration // the host takes to say its time
		skew   time.Duration // the host's clock ahead of this one
		silent bool          // the host does not say
		bare   bool          // its record has no time
		want   bool
	}{
		{"looked at within the time", 3 * time.Second, 5 * time.Second, time.Second, 0, 0, false, false, true},
		{"looked at within the time, its record without one", 3 * time.Second, 5 * time.Second, time.Second, 0, 0, false, true, true},
		{"ran in time, found an hour on", 10 * time.Second, time.Hour, time.Second, 300 * time.Millisecond, 0, false, false, true},
		{"ran in time, found an hour on, the host's clock ahead", 10 * time.Second, time.Hour, time.Second, 300 * time.Millisecond, 7 * time.Minute, false, false, true},
		{"ran in time, found an hour on, the host's clock behind", 10 * time.Second, time.Hour, time.Second, 300 * time.Millisecond, -7 * time.Minute, false, false, true},
		{"ran a minute after, found later", time.Minute, time.Hour, time.Second, 300 * time.Millisecond, 0, false, false, false},
		{"ran a minute after, the host's clock ahead", time.Minute, time.Hour, time.Second, 300 * time.Millisecond, 7 * time.Minute, false, false, false},
		{"ran a minute after, the host's clock behind", time.Minute, time.Hour, time.Second, 300 * time.Millisecond, -7 * time.Minute, false, false, false},
		{"ran at 27 s, the look begun at 30 s, its log read for 8 s", 27 * time.Second, 30 * time.Second, 8 * time.Second, 300 * time.Millisecond, 0, false, false, false},
		{"ran at 27 s, the host 8 s to say its time: not known", 27 * time.Second, 30 * time.Second, 0, 8 * time.Second, 0, false, false, false},
		{"ran in time, the host slow to say its time: not known", 10 * time.Second, time.Hour, 0, 8 * time.Second, 0, false, false, false},
		{"ran at 22 s, the look begun at 19 s, its log read for 4 s", 22 * time.Second, 19 * time.Second, 4 * time.Second, 300 * time.Millisecond, 0, false, false, false},
		{"found later, the host does not say", 10 * time.Second, time.Hour, 0, 0, 0, true, false, false},
		{"found later, its record without a time", 10 * time.Second, time.Hour, 0, 0, 0, false, true, false},
	} {
		begun := t0.Add(c.look)
		cur := begun.Add(c.read) // (its log was read before this is asked)
		w := &lineWatch{typedAt: t0, log: &lineLog{end: lineRan}}
		if !c.bare {
			w.log.at = t0.Add(c.ran + c.skew)
		}
		p := lineProbe{clock: func() time.Time { return cur }, hostNow: func() (time.Time, bool) {
			cur = cur.Add(c.ask)
			return cur.Add(c.skew - c.ask/2), !c.silent
		}}
		if got := lineOurs(p, w, begun); got != c.want {
			t.Errorf("%s: ours %v, want %v", c.what, got, c.want)
		}
	}
}

// A default not put back at once is put back later only where the
// settings were not written since the line ran: the same word saved by
// another session an hour on is theirs. And it is not tried for ever.
func TestLineConcludeRetry(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	host := func(at time.Time) time.Time { return at.Add(7 * time.Minute) } // (the host's clock, ahead)
	for _, c := range []struct {
		what     string
		written  time.Duration // the settings last written, after the line was typed
		look     time.Duration
		writeErr error
		stampErr error
		res      settled
		final    bool
		model    string
	}{
		{"written by the line, found an hour on: put back", time.Second, time.Hour, nil, nil, settleBack, true, `"opus"`},
		{"the same word saved by another session since: theirs", 30 * time.Minute, time.Hour, nil, nil, settleForeign, true, `"haiku"`},
		{"written a second after its record (their time in whole seconds): still the line's", 2 * time.Second, time.Minute, nil, nil, settleBack, true, `"opus"`},
		{"when they were written not told: not put back yet", time.Second, time.Minute, nil, errors.New("no stat"), settleFailed, false, `"haiku"`},
		{"not written back yet: tried again later", time.Second, time.Minute, errors.New("denied"), nil, settleFailed, false, `"haiku"`},
		{"still not, long after: given up", time.Second, lineGiveUp + time.Second, errors.New("denied"), nil, settleFailed, true, `"haiku"`},
	} {
		where := fakeWhere{log: stamped(lineRecords("sid-1", "c1", "haiku", "Haiku 5.5"), host(t0.Add(time.Second)))}
		now := t0.Add(c.look)
		where.clock = func() time.Time { return host(now) }
		st := fakeSettings{doc: `{"model": "haiku"}`, mtime: host(t0.Add(c.written)), writeErr: c.writeErr, stampErr: c.stampErr}
		w := &lineWatch{had: true, was: rawJSON(`"opus"`), wrote: rawJSON(`"haiku"`), log: &lineLog{sid: "sid-1", arg: "haiku"}, typedAt: t0}
		end, res, final, _, _ := lineConclude(where.probe(), st.io(), w, now)
		if end != lineRan || res != c.res || final != c.final || !jsonSame(rawJSON(st.model()), rawJSON(c.model)) {
			t.Errorf("%s: end %v settled %v final %v, the model %s; want %v %v %s", c.what, end, res, final, st.model(), c.res, c.final, c.model)
		}
	}
}

// The picker as a subscription's Claude Code draws it (2.1.322, measured on
// a real one 2026-11-11): its rows are named with their version, and what
// is beside them says nothing of it.
func TestLineModelArgVersionedRows(t *testing.T) {
	screen := "   Select model\n   Switch between Claude models. Your pick becomes the default for new sessions.\n\n     1.  Default (recommended)  Opus 5.5 · Best for everyday, complex tasks\n   ❯ 2.  Opus 5.5 ✔             For complex work and everyday tasks\n     3.  Fable 5.2              For your toughest challenges\n     4.  Sonnet 5.5             Most efficient for simpler tasks\n     5.  Haiku 5.5              Fastest for quick answers\n     ▸   More models\n\n   ● High effort ←/→ to adjust\n\n   Enter to set as default · s to use this session only · Esc to cancel\n"
	rows := parseModelPicker(screen).offered()
	if len(rows) != 4 {
		t.Fatalf("rows read: %+v", rows)
	}
	now := time.Now()
	key := lineListKey("acct-versioned", 1, 1)
	t.Cleanup(func() { lineLists.Delete(key) })
	noteOffered(key, rows, now)
	for label, want := range map[string][2]string{"Opus 5.5": {"opus", "Opus 5.5"}, "Fable 5.2": {"fable", "Fable 5.2"}, "Sonnet 5.5": {"sonnet", "Sonnet 5.5"}, "Haiku 5.5": {"haiku", "Haiku 5.5"}} {
		arg, name, ok := lineModelArg(key, label, now)
		if !ok || arg != want[0] || name != want[1] {
			t.Errorf("%s: %q %q %v, want %q %q", label, arg, name, ok, want[0], want[1])
		}
	}
	// (Two rows of one family: the family's word could be either — neither is typed.)
	two := append(append([]modelOpt{}, rows...), modelOpt{Label: "Opus 5.1", Desc: "An earlier Opus"})
	noteOffered(key, two, now)
	for _, label := range []string{"Opus 5.5", "Opus 5.1"} {
		if _, _, ok := lineModelArg(key, label, now); ok {
			t.Errorf("%s: typed on a line with two rows of its family listed", label)
		}
	}
	if _, _, ok := lineModelArg(key, "Sonnet 5.5", now); !ok {
		t.Error("Sonnet 5.5: not typed on a line though alone of its family")
	}
	// (One of the two taken in the picker itself: still one of two.)
	confirmOffered(key, lineKinKey("acct-versioned", "", ""), modelOpt{Label: "Opus 5.1", Desc: "An earlier Opus"}, now)
	for _, label := range []string{"Opus 5.5", "Opus 5.1"} {
		if _, _, ok := lineModelArg(key, label, now); ok {
			t.Errorf("%s: typed on a line after one of its family's two rows was taken in the picker", label)
		}
	}
	// (Only "Opus 5.1" known, then "Opus 5.5" taken in the picker: the one known before is one of two now.)
	noteOffered(key, []modelOpt{{Label: "Opus 5.1", Desc: "An earlier Opus"}}, now)
	if _, _, ok := lineModelArg(key, "Opus 5.1", now); !ok {
		t.Fatal("Opus 5.1 alone: not typed on a line")
	}
	confirmOffered(key, lineKinKey("acct-versioned", "", ""), modelOpt{Label: "Opus 5.5", Desc: "For complex work"}, now)
	for _, label := range []string{"Opus 5.5", "Opus 5.1"} {
		if _, _, ok := lineModelArg(key, label, now); ok {
			t.Errorf("%s: typed on a line once a second row of its family was taken in the picker", label)
		}
	}
	// (A row of the same name kept with an earlier version beside it: the name to expect is the one just shown.)
	noteOffered(key, []modelOpt{{Label: "Opus", Desc: "Opus 5.1 · Earlier"}}, now)
	confirmOffered(key, lineKinKey("acct-versioned", "", ""), modelOpt{Label: "Opus", Desc: "Opus 5.5 · Best for everyday"}, now)
	if _, name, ok := lineModelArg(key, "Opus", now); !ok || name != "Opus 5.5" {
		t.Errorf("Opus after its row was shown anew: %q %v, want Opus 5.5", name, ok)
	}
	// (A variant of the same model listed beside it is a second row of the family.)
	for _, other := range []string{"Opus 5.5 (1M context)", "Opus (1M context)", "Opus 5.5 [1m]"} {
		noteOffered(key, []modelOpt{{Label: "Opus 5.5", Desc: "For complex work"}, {Label: other, Desc: "For long sessions"}, {Label: "Sonnet 5.5", Desc: "Efficient"}}, now)
		if _, _, ok := lineModelArg(key, "Opus 5.5", now); ok {
			t.Errorf("Opus 5.5: typed on a line with %q listed beside it", other)
		}
		if _, _, ok := lineModelArg(key, "Sonnet 5.5", now); !ok {
			t.Errorf("Sonnet 5.5: not typed on a line with %q listed (another family's)", other)
		}
		confirmOffered(key, lineKinKey("acct-versioned", "", ""), modelOpt{Label: other, Desc: "For long sessions"}, now)
		if _, _, ok := lineModelArg(key, "Opus 5.5", now); ok {
			t.Errorf("Opus 5.5: typed on a line after %q was taken in the picker", other)
		}
	}
	// (A row of the family whose version is not said counts as one of its rows.)
	noteOffered(key, []modelOpt{{Label: "Opus 5.1", Desc: "An earlier Opus"}, {Label: "Opus", Desc: "For complex work"}}, now)
	if _, _, ok := lineModelArg(key, "Opus 5.1", now); ok {
		t.Error("Opus 5.1: typed on a line with another Opus row, of no version said, listed")
	}
	// (The log's mark of the account's default is not another model; another version is.)
	for said, same := range map[string]bool{"Opus 5.5": true, "Opus 5.5 (default)": true, "Opus 5.1": false, "Opus 5.1 (default)": false, "Sonnet 5.5": false} {
		if lineSame(said, "Opus 5.5") != same {
			t.Errorf("lineSame(%q, Opus 5.5) = %v", said, !same)
		}
	}
	// (Not a model's name with a version: never typed.)
	for _, label := range []string{"Default (recommended)", "More models", "opus 5.5", "Gpt 6"} {
		if _, ok := lineWord(label); ok {
			t.Errorf("%q has a word", label)
		}
	}
}

func TestNoteModelFrontLetsGoOfOld(t *testing.T) {
	now := time.Now()
	old, fresh, added := modelFrontKey("zz-g", "p", 1, 1, "s"), modelFrontKey("zz-g", "p", 2, 2, "s"), modelFrontKey("zz-g", "p", 3, 3, "s")
	t.Cleanup(func() { modelFronts.Delete(old); modelFronts.Delete(fresh); modelFronts.Delete(added) })
	modelFronts.Store(old, modelFront{at: now.Add(-lineListFor - time.Minute)})
	modelFronts.Store(fresh, modelFront{at: now.Add(-time.Hour)})
	noteModelFront(added, modelFront{via: "v", auth: "a", at: now})
	if _, ok := modelFronts.Load(old); ok {
		t.Error("one kept too long ago is still kept")
	}
	for _, k := range []string{fresh, added} {
		if _, ok := modelFronts.Load(k); !ok {
			t.Errorf("%q let go of", k)
		}
	}
}
