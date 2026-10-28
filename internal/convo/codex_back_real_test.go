package convo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexBackReal prints what each backtrack in the rollouts under
// CORRAL_CODEX_LOGS took back, for reading by eye. Skipped without it.
func TestCodexBackReal(t *testing.T) {
	glob := os.Getenv("CORRAL_CODEX_LOGS")
	if glob == "" {
		t.Skip("CORRAL_CODEX_LOGS not set")
	}
	files, _ := filepath.Glob(glob)
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil || !strings.Contains(string(b), `"thread_rolled_back"`) {
			continue
		}
		c := NewCodex()
		c.Feed(b)
		var gone, after []string
		for _, e := range c.entries {
			s := ""
			if len(e.Parts) > 0 {
				s = e.Parts[0].Text
				if e.Parts[0].Tool != nil {
					s = "[" + e.Parts[0].Tool.Name + "]"
				}
			}
			s = e.Role[:1] + ":" + strings.ReplaceAll(s[:min(len(s), 24)], "\n", " ")
			if e.Gone {
				gone = append(gone, s)
			} else if len(gone) > 0 && len(after) < 2 {
				after = append(after, s)
			}
		}
		fmt.Printf("%s gone=%d %q then %q\n", filepath.Base(name)[30:44], len(gone), gone[:min(len(gone), 4)], after)
	}
}
