package convo

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeAnswerReal reads the end of each real log under
// CORRAL_ANSWER_LOGS, as much of it as the gateway takes in when a
// conversation is opened, and says what it makes of the latest answer
// there: its tokens, how long it took and the rate of the two. Against
// the same worked out of the same logs another way (the devtools'
// turns.py), so that one reading of these rows is not the only one.
// Skipped without the variable.
func TestClaudeAnswerReal(t *testing.T) {
	glob := os.Getenv("CORRAL_ANSWER_LOGS")
	if glob == "" {
		t.Skip("CORRAL_ANSWER_LOGS not set")
	}
	const tail = 4 << 20 // (convoTail: what the gateway reads at first)
	files, _ := filepath.Glob(glob)
	if len(files) == 0 {
		t.Fatalf("no log at %s", glob)
	}
	told := 0 // logs whose latest answer was read
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(name), err)
			continue
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			continue
		}
		at := max(int64(0), fi.Size()-tail)
		b := make([]byte, min(fi.Size(), int64(tail)))
		_, err = f.ReadAt(b, at)
		f.Close()
		if err != nil && err != io.EOF {
			t.Errorf("%s: %v", filepath.Base(name), err)
			continue
		}
		if at > 0 { // (the line the read began in the middle of)
			if i := bytes.IndexByte(b, '\n'); i >= 0 {
				b = b[i+1:]
			}
		}
		c := NewClaude()
		c.Feed(b)
		a := c.Meta().Answer
		if a == nil {
			// (A log may hold no answer at its end — one the agent only
			// read files in, one of a kind this does not read: said, as it
			// is worth knowing how many, but not a fault of itself.)
			t.Logf("%s: no answer in the last %d bytes", filepath.Base(name), len(b))
			continue
		}
		told++
		rate := 0.0
		if a.Ms > 0 {
			rate = float64(a.Out) / (float64(a.Ms) / 1000)
		}
		t.Logf("%s: out=%d ms=%d rate=%.1f tok/s", filepath.Base(name), a.Out, a.Ms, rate)
		// (What a model writing cannot be: nothing at all, or faster than
		// any of them goes. A log read in part may say nothing of the time,
		// which is not an error — only a rate that could not have happened.)
		if a.Out <= 0 || rate < 0 || rate > 2000 {
			t.Errorf("%s: out=%d ms=%d rate=%.1f", filepath.Base(name), a.Out, a.Ms, rate)
		}
	}
	// (Else the run said nothing of anything and passed for it.)
	if told == 0 {
		t.Fatalf("not one of the %d logs had an answer at its end", len(files))
	}
}
