package convo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeTreeRealDump writes, for each log under CORRAL_TREE_LOGS, the
// entries kept and those taken back, for a check against another reading
// of the same logs (see the devtools' tree/compare.py). Skipped without it.
func TestClaudeTreeRealDump(t *testing.T) {
	glob, out := os.Getenv("CORRAL_TREE_LOGS"), os.Getenv("CORRAL_TREE_OUT")
	if glob == "" || out == "" {
		t.Skip("CORRAL_TREE_LOGS and CORRAL_TREE_OUT not set")
	}
	files, _ := filepath.Glob(glob)
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		c := NewClaude()
		c.Feed(b)
		var ids, gone []string
		for _, e := range c.entries {
			ids = append(ids, e.ID)
			if e.Gone {
				gone = append(gone, e.ID)
			}
		}
		_ = enc.Encode(map[string]any{"file": name, "ids": ids, "gone": gone})
	}
}
