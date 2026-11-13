package web

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPickerAdversarialBoundaries(t *testing.T) {
	rows := "  ❯ 8. Opus 5    Best for tasks\n    9. Haiku 5   Quick answers\n"
	for name, screen := range map[string]string{
		"title alone":          "Select model\n" + rows,
		"prompt without rule":  "Select model\n" + rows + "❯ draft\n↓",
		"numbered input draft": "Select model\n" + rows + "──────────\n❯ 10. draft\n──────────\n↓",
		"keys before title":    pickerKeys + "\nSelect model\n" + rows,
		"repeated cursor":      "Select model\n" + strings.Replace(rows, "    9.", "  ❯ 9.", 1) + pickerKeys,
		"gapped numbers":       "Select model\n" + strings.Replace(rows, "9.", "10.", 1) + pickerKeys,
		"repeated default":     "Select model\n  1. Default\n" + "  1. Default\n" + rows + pickerKeys,
		"dialog below keys":    "Select model\n" + rows + pickerKeys + "\nDo you want to proceed?\n❯ 1. Yes\n  2. No",
	} {
		t.Run(name, func(t *testing.T) {
			if p := parseModelPicker(screen); p.ok || pickerShows(screen) {
				t.Fatalf("accepted ambiguous or malformed picker: %+v", p)
			}
		})
	}
	for name, screen := range map[string]string{
		"cut head starts at eight": rows + pickerKeys,
		"cut foot":                 "Select model\n" + rows + "  … +5 models\n  ● High effort (default) ←/→ to adjust     ↓",
		"cursor scrolled off":      "Select model\n" + strings.ReplaceAll(rows, "❯", " ") + "↓",
		"arrow on model row":       "Select model\n    8. Opus 5    Best for tasks     ↓",
	} {
		t.Run(name, func(t *testing.T) {
			p := parseModelPicker(screen)
			if !p.ok || !pickerShows(screen) || p.offered()[0].N != 8 || p.offered()[0].Label != "Opus 5" || p.offered()[0].Desc != "Best for tasks" {
				t.Fatalf("did not read clipped picker: %+v", p)
			}
		})
	}
}

func TestPickerExpansionAndSummaryAreNotModels(t *testing.T) {
	screen := "Select model\n    7. Opus 5    Best for tasks\n  ❯ 8. More models    Show 8 more\n    … +5 models\n  ● High effort (default) ←/→ to adjust     ↓"
	p := parseModelPicker(screen)
	c, known := p.cursor()
	if !p.ok || len(p.offered()) != 1 || !known || c.N != 8 || c.Label != "" {
		t.Fatalf("expansion row offered as a model or cursor lost: %+v", p)
	}
	// A summary aligned with the description must not be appended to it.
	screen = "Select model\n    7. Opus 5    Best for tasks\n                … +5 models\n↓"
	if p = parseModelPicker(screen); !p.ok || p.offered()[0].Desc != "Best for tasks" {
		t.Fatalf("summary became description: %+v", p)
	}
	for _, target := range []string{"More models", "Opus…"} {
		f := newFake(false)
		f.models = append(f.models, target)
		f.current = len(f.models) - 1
		if _, err := modelFlow(f.step, f.step, target, ""); !errors.Is(err, errNoModel) {
			t.Fatalf("%q: %v", target, err)
		}
		if f.session != "" || f.saved || strings.Contains(strings.Join(f.pressed, ","), "this") {
			t.Fatalf("selected non-model %q: %+v", target, f)
		}
	}
}

func TestDescriptionAmbiguousFullWidthBoundary(t *testing.T) {
	// At column 30, width 15: either a long word was cut, or a 15-letter
	// word ended there. The capture cannot say which original was used,
	// so the words are joined with a space either way — the menu is not
	// that pane and wraps them itself.
	screen := "Select model\n" + fmt.Sprintf("    2. %-23s%s\n", "Fable", "longest-running") + strings.Repeat(" ", 30) + "tasks\n" + pickerKeys
	p := parseModelPicker(screen)
	if !p.ok || p.offered()[0].Desc != "longest-running tasks" {
		t.Fatalf("invented a word boundary: %+v", p)
	}
}

func TestDescriptionStartsWithLabel(t *testing.T) {
	screen := "Select model\n    2. Opus    Opus\n               description\n" + pickerKeys
	p := parseModelPicker(screen)
	if !p.ok || p.offered()[0].Desc != "Opus description" {
		t.Fatalf("description column confused with label: %+v", p)
	}
}

func TestTakenRequiresCommandResult(t *testing.T) {
	phrase := "Set model to Haiku 5.5 for this session only"
	for _, prefix := range []string{"", "● ", "❯ ", "You should see: "} {
		if taken(prefix+phrase, "Haiku 5.5") != 0 {
			t.Fatalf("counted prose with prefix %q", prefix)
		}
	}
	if taken("  ⎿ "+phrase+" with xhigh effort", "Haiku 5.5") != 1 {
		t.Fatal("lost real confirmation with an effort suffix")
	}
}

func TestModelFlowDoesNotSucceedWithPickerStillOpen(t *testing.T) {
	f := newFake(false)
	stuck := func(keys []string, text string, read int) (string, error) {
		s, err := f.step(keys, text, read)
		if len(keys) == 1 && keys[0] == "this" {
			f.open = true
			s = f.screen() // A confirmation in the thread cannot override this.
		}
		return s, err
	}
	if _, err := modelFlow(stuck, f.step, "Haiku 5.5", ""); !errors.Is(err, errNotTaken) {
		t.Fatalf("reported success with picker open: %v", err)
	}
}

func TestModelFlowListMustCloseDialog(t *testing.T) {
	f := newFake(false)
	surprise := func(keys []string, text string, read int) (string, error) {
		s, err := f.step(keys, text, read)
		if len(keys) == 1 && keys[0] == "esc" {
			f.open, f.asking = true, true
			s = f.screen()
		}
		return s, err
	}
	if _, err := modelFlow(surprise, f.step, "", ""); !errors.Is(err, errScreen) || f.asking || f.open {
		t.Fatalf("returned models with a dialog left open: %v, %+v", err, f)
	}
}

func TestModelFlowFindsClippedCursor(t *testing.T) {
	f := newFake(false)
	f.current = 4 // The viewport shows rows 1–3, but the cursor is on row 5.
	clipped := func(keys []string, text string, read int) (string, error) {
		s, err := f.step(keys, text, read)
		if !f.open || f.asking || err != nil {
			return s, err
		}
		var b strings.Builder
		b.WriteString("Select model\n")
		for i, label := range f.models[:3] {
			mark := " "
			if i == f.cursor {
				mark = "❯"
			}
			fmt.Fprintf(&b, "  %s %d. %s    Description\n", mark, i+1, label)
		}
		b.WriteString("  … +2 models\n  ● High effort (default) ←/→ to adjust     ↓")
		return b.String(), nil
	}
	if _, err := modelFlow(clipped, f.step, "Opus 5.5", ""); err != nil || f.session != "Opus 5.5" || f.saved {
		t.Fatalf("clipped cursor: %v, %+v", err, f)
	}
}

func TestModelFlowFindsCursorClippedAbove(t *testing.T) {
	f := newFake(false)
	f.current = 0
	clipped := func(keys []string, text string, read int) (string, error) {
		s, err := f.step(keys, text, read)
		if !f.open || f.asking || err != nil {
			return s, err
		}
		var b strings.Builder
		for i := 2; i < len(f.models); i++ {
			mark := " "
			if i == f.cursor {
				mark = "❯"
			}
			fmt.Fprintf(&b, "  %s %d. %s    Description\n", mark, i+1, f.models[i])
		}
		b.WriteString(pickerKeys)
		return b.String(), nil
	}
	if _, err := modelFlow(clipped, f.step, "Haiku 5.5", ""); err != nil || f.session != "Haiku 5.5" || f.saved {
		t.Fatalf("cursor clipped above: %v, %+v", err, f)
	}
}

func TestAskQuotedAboveInputIsNotConfirmation(t *testing.T) {
	question := "Switch model?\n❯ 1. Yes, switch to Haiku 5.5\n  2. No, go back\n"
	if askShows(question+ownerMain, "Haiku 5.5") {
		t.Fatal("quoted confirmation above input would receive Enter")
	}
	if askShows(question+"Do you want to proceed?\n❯ 1. Yes\n  2. No", "Haiku 5.5") {
		t.Fatal("quoted confirmation above another dialog would receive Enter")
	}
}

// Run the actual gate_enter shell body with controlled host state. This
// exercises Escape ordering without a terminal, SSH server or network.
func TestPickerEnterGate(t *testing.T) {
	cmd := convoGatedCommand("free", "herdr", "", "p1", "sock", 1, 1, sidA, 6, nil, 0)
	raw, err := exec.Command("sh", "-c", `sh() { printf '%s' "$2"; }; `+cmd).Output()
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "gate_enter() {")
	if start < 0 {
		t.Fatal("gate_enter not found")
	}
	end := strings.Index(script[start:], "\nif [ -n \"$gate\" ]; then")
	if end < 0 {
		t.Fatal("gate_enter not found")
	}
	gate := script[start : start+end]
	for _, mode := range []string{"herdr", "tmux"} {
		for _, tc := range []struct {
			name, owner, state, why, screen string
			front                           bool
			want                            int
			escape                          bool
		}{
			{"palette", "none", "idle", "", "    /model Switch model\n    /effort Effort", true, 0, true},
			{"one selected match", "none", "waiting", "dialog open", "  ❯ /model Switch model", true, 0, true},
			{"wrapped description", "none", "idle", "", "    /model Switch the\n           model", true, 0, true},
			{"question over old commands", "none", "waiting", "permission prompt", " /model Switch\n /effort Effort", true, 5, false},
			{"process changed", "none", "idle", "", " /model Switch\n /effort Effort", false, 16, false},
			{"subagent", "subagent", "idle", "", " /model Switch\n /effort Effort", true, 16, false},
			{"list", "list", "idle", "", " /model Switch\n /effort Effort", true, 16, false},
			{"commands quoted above dialog", "none", "idle", "", "    /model Switch\n    /effort Effort\nSwitch model?\n❯ 1. Yes", true, 16, false},
			{"main input", "main", "idle", "", ownerMain, true, 0, false},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				log := filepath.Join(t.TempDir(), "keys")
				stubs := `
kind=claude cx= gate=free cmd1=/ p=p1 sock=sock escaped=
front() { [ "$CASE_FRONT" = true ]; }
waiting() { st=$CASE_STATE wf=$CASE_WHY; [ -z "$escaped" ] || { st=idle; wf=; }; }
owner() { o=$CASE_OWNER os=$CASE_SCREEN; [ -z "$escaped" ] || o=main; }
owned() { owner 1; [ "$o" = main ] || exit 16; }
sleep() { :; }
herdr() { printf '%s\n' "$*" >>"$CASE_LOG"; escaped=1; }
tmux() { printf '%s\n' "$*" >>"$CASE_LOG"; escaped=1; }
`
				c := exec.Command("sh", "-c", stubs+gate+"\ngate_enter\n")
				c.Env = append(os.Environ(), "mode="+mode, "CASE_FRONT="+fmt.Sprint(tc.front), "CASE_STATE="+tc.state, "CASE_WHY="+tc.why, "CASE_OWNER="+tc.owner, "CASE_SCREEN="+tc.screen, "CASE_LOG="+log)
				out, err := c.CombinedOutput()
				code := 0
				if err != nil {
					var ee *exec.ExitError
					if !errors.As(err, &ee) {
						t.Fatal(err)
					}
					code = ee.ExitCode()
				}
				pressed, _ := os.ReadFile(log)
				if code != tc.want || (len(pressed) > 0) != tc.escape {
					t.Fatalf("exit %d, keys %q, output %s; want %d, Escape %v", code, pressed, out, tc.want, tc.escape)
				}
			})
		}
	}
}
