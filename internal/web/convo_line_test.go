package web

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeLine stands in for a Claude Code taking "/model NAME" on one line,
// as the real one does (out-e10, out-e11, out-e14): any name is taken, the
// default of new sessions is saved, its log says so a moment later, and
// with turns behind it, it asks first.
type fakeLine struct {
	names   map[string]string // what a word typed is the name of
	history bool              // it asks "Switch model?" first
	late    int               // reads of the log before its record shows
	silent  bool              // its log never says
	asksFor string            // (its question names this, not what was typed)
	draft   string
	busy    bool
	open    bool // a picker left open

	asking  string // the name its question is for
	log     strings.Builder
	pending string
	reads   int
	typed   []string
	pressed []string
	saved   string // the settings' "model"
	session string
}

func newFakeLine() *fakeLine {
	return &fakeLine{names: map[string]string{"opus": "Opus 5.5", "sonnet": "Sonnet 5.5", "haiku": "Haiku 5.5", "fable": "Fable 5.2"}}
}

func (f *fakeLine) screen() string {
	var b strings.Builder
	if f.busy {
		b.WriteString("✻ Crunching… (12s · esc to interrupt)\n")
	}
	b.WriteString("────────────────────────\n❯ " + f.draft + "\n────────────────────────\n  Opus 5.5 · /tmp\n")
	if f.open {
		b.WriteString("   Select model\n   ❯ 1.  Default (recommended)\n   Enter to set as default · s to use this session only · Esc to cancel\n")
	}
	if f.asking != "" {
		b.WriteString("   Switch model?\n   Your next response will be slower and use more tokens\n\n   ❯ 1. Yes, switch to " + f.asking + "\n     2. No, go back\n")
	}
	return b.String()
}

func (f *fakeLine) take(name, word string) {
	f.session, f.saved, f.asking = name, word, ""
	f.pending = fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>Set model to `+"`%s`"+` and saved as your default for new sessions</local-command-stdout>"}}`+"\n", name)
	f.reads = 0
}

func (f *fakeLine) io() lineIO {
	word := ""
	return lineIO{
		look: func() (string, error) { return f.screen(), nil },
		typed: func(text string) error {
			f.typed = append(f.typed, text)
			word = strings.ToLower(strings.TrimPrefix(text, "/model "))
			name := f.names[word]
			if name == "" {
				name = word // (a name it does not know: taken as it is)
			}
			if f.asksFor != "" {
				name = f.asksFor
			}
			if f.history {
				f.asking = name
				return nil
			}
			f.take(name, word)
			return nil
		},
		press: func(keys []string) (string, error) {
			for _, k := range keys {
				f.pressed = append(f.pressed, k)
				switch {
				case f.asking != "" && k == "yes":
					f.take(f.asking, word)
				case f.asking != "" && k == "esc":
					f.asking = ""
				}
			}
			return f.screen(), nil
		},
		logSize: func() (int64, error) { return int64(f.log.Len()), nil },
		logFrom: func(off int64) ([]byte, error) {
			if f.pending != "" && !f.silent {
				if f.reads++; f.reads > f.late {
					f.log.WriteString(f.pending)
					f.pending = ""
				}
			}
			return []byte(f.log.String()[off:]), nil
		},
		sleep: func(time.Duration) {},
	}
}

func TestLineModelArg(t *testing.T) {
	for label, want := range map[string]string{
		"Haiku": "haiku", "Haiku 5.5": "haiku", "Opus": "opus", "Fable 5.2": "fable", "Sonnet 5": "sonnet",
		"Opus 5.5 (1M context)": "", "Default (recommended)": "", "nonsense-xyz-1": "", "haiku": "", "GPT 6": "", "Opus\n": "", "": "",
	} {
		if got, ok := lineModelArg(label); got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", label, got, ok, want)
		}
	}
}

func TestLineTook(t *testing.T) {
	said := func(s string) string {
		return `{"type":"user","message":{"role":"user","content":"<local-command-stdout>` + s + `</local-command-stdout>"}}`
	}
	for _, c := range []struct {
		log, want string
	}{
		{said("Set model to `Haiku 5.5` and saved as your default for new sessions"), "Haiku 5.5"},
		{said("Set model to `Opus 5.5`") + "\n" + said("Set model to `Sonnet 5.5`"), "Sonnet 5.5"},
		{said("Set effort level to low"), ""},
		// (The same words quoted by the person, or said by the model: not its answer.)
		{`{"type":"user","message":{"role":"user","content":"it said <local-command-stdout>Set model to ` + "`Haiku 5.5`" + `"}}`, ""},
		{`{"type":"assistant","message":{"role":"assistant","content":"<local-command-stdout>Set model to ` + "`Haiku 5.5`" + `"}}`, ""},
		{`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<local-command-stdout>Set model to ` + "`Haiku 5.5`" + `"}]}}`, ""},
		{said("Set model to `Haiku 5.5` and")[:60], ""}, // (a record not whole yet)
	} {
		if got, ok := lineTook([]byte(c.log)); got != c.want || ok != (c.want != "") {
			t.Errorf("%.70s: %q %v, want %q", c.log, got, ok, c.want)
		}
	}
}

func TestModelLineFlowSwitches(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, label := range []string{"Haiku", "Haiku 5.5", "Opus", "Fable 5.2"} {
			f := newFakeLine()
			f.history, f.late = history, 3
			arg, _ := lineModelArg(label)
			name, err := modelLineFlow(f.io(), label, arg)
			if err != nil || !strings.HasPrefix(f.session, strings.Fields(label)[0]) || name != f.session {
				t.Fatalf("%v %s: %q %v (session %q)", history, label, name, err, f.session)
			}
			if len(f.typed) != 1 || f.typed[0] != "/model "+arg {
				t.Errorf("typed %v", f.typed)
			}
			if yes := strings.Count(strings.Join(f.pressed, ","), "yes"); yes != map[bool]int{false: 0, true: 1}[history] {
				t.Errorf("%v %s: Enter pressed %d times (%v)", history, label, yes, f.pressed)
			}
		}
	}
}

// Nothing is typed unless it is at rest, its input empty, nothing of its own open.
func TestModelLineFlowRefuses(t *testing.T) {
	for name, set := range map[string]func(*fakeLine){
		"busy":   func(f *fakeLine) { f.busy = true },
		"draft":  func(f *fakeLine) { f.draft = "half a thought" },
		"picker": func(f *fakeLine) { f.open = true },
		"asking": func(f *fakeLine) { f.asking = "Opus 5.5" },
	} {
		f := newFakeLine()
		set(f)
		if _, err := modelLineFlow(f.io(), "Haiku", "haiku"); err == nil || len(f.typed) != 0 || len(f.pressed) != 0 {
			t.Errorf("%s: %v, typed %v pressed %v", name, err, f.typed, f.pressed)
		}
	}
}

// What it took is what its log says, not what was asked of it.
func TestModelLineFlowElsewhere(t *testing.T) {
	f := newFakeLine()
	f.names["haiku"] = "Sonnet 5.5"
	if name, err := modelLineFlow(f.io(), "Haiku", "haiku"); !errors.Is(err, errElsewhere) || name != "Sonnet 5.5" {
		t.Fatalf("%q %v", name, err)
	}
	// Its question for another model: not answered with Enter, left with Escape.
	f = newFakeLine()
	f.history, f.asksFor = true, "Sonnet 5.5"
	if _, err := modelLineFlow(f.io(), "Haiku 5.5", "haiku"); !errors.Is(err, errNotTaken) || f.session != "" || strings.Join(f.pressed, ",") != "esc" {
		t.Fatalf("%v, session %q, pressed %v", err, f.session, f.pressed)
	}
}

// Its log silent: said as not known, never as taken.
func TestModelLineFlowUnsure(t *testing.T) {
	f := newFakeLine()
	f.silent = true
	if _, err := modelLineFlow(f.io(), "Haiku", "haiku"); !errors.Is(err, errUnsure) {
		t.Fatal(err)
	}
	// A record from before the line was typed is not its answer.
	f = newFakeLine()
	f.log.WriteString(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>Set model to ` + "`Haiku 5.5`" + `</local-command-stdout>"}}` + "\n")
	f.silent = true
	if _, err := modelLineFlow(f.io(), "Haiku", "haiku"); !errors.Is(err, errUnsure) {
		t.Fatal(err)
	}
}
