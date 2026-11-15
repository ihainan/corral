package convo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexView: what a reader of a Codex log shows — its words in turn (each
// entry's, but their numbering, which a part read later renumbers), each
// step's state by its id (where a step whose call and end fall in two parts
// stands is the later part's, by design: Prepend), what is running, its
// status, title and tasks.
func codexView(c *Codex) string {
	type step struct {
		Name, Detail, Result string
		Done, Error          bool
	}
	var words []string
	steps := map[string]step{}
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Tool != nil {
				t := p.Tool
				steps[t.ID] = step{t.Name, t.Detail, t.Result, t.Done, t.Error}
				continue
			}
			b, _ := json.Marshal(p)
			words = append(words, e.Role+" "+string(b))
		}
		if e.Gone {
			words = append(words, "(gone)")
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"words": words, "steps": steps, "running": c.Running(), "status": c.Status(), "title": c.Title(), "tasks": c.Tasks()}, "", " ")
	return string(b)
}

// A long log is read from its end, and what came before when it is
// scrolled to (Prepend): read so, cut at any row, it must show what it shows
// read whole. Real Codex 0.167.2 logs (the test container, a mock model,
// 11-15; long strings cut short): code mode, every kind of row it wrote
// (background commands, polls, subagents, compaction, interrupts, images,
// retries); function calls (code mode off: exec_command, write_stdin).
// (Review rounds 2–4 of the background commands' work were all of this
// kind: what one part knows the other must too.)
func TestCodexSplitAnywhere(t *testing.T) {
	files, _ := filepath.Glob("testdata/codex-rollouts/*.jsonl")
	if len(files) < 3 {
		t.Fatalf("%d logs", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rows := strings.SplitAfter(strings.TrimRight(string(b), "\n"), "\n")
		whole := codexView(feedAll(rows...))
		bad := 0
		for k := 1; k < len(rows) && bad < 3; k++ {
			c := feedAll(rows[k:]...)
			c.Prepend(feedAll(rows[:k]...))
			if got := codexView(c); got != whole {
				bad++
				t.Errorf("%s cut before row %d (%.80s): %s", filepath.Base(f), k+1, rows[k], firstDiff(got, whole))
			}
		}
	}
}

// firstDiff: where two views part, a little either side.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			lo := max(0, i-3)
			return "\n split: " + strings.Join(al[lo:min(len(al), i+2)], "\n        ") + "\n whole: " + strings.Join(bl[lo:min(len(bl), i+2)], "\n        ")
		}
	}
	return "lengths differ"
}
