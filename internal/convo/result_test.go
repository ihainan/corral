package convo

import (
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"
)

func TestSetResultShort(t *testing.T) {
	var tl Tool
	tl.setResult("ok\n")
	if tl.Result != "ok\n" || tl.ResultTail != "" || tl.Truncated || tl.Omitted != 0 {
		t.Fatalf("%+v", tl)
	}
}

func TestSetResultHeadAndTail(t *testing.T) {
	var lines []string
	for i := range 2000 {
		lines = append(lines, strings.Repeat("x", 20)+" line "+string(rune('a'+i%26)))
	}
	lines[0], lines[len(lines)-1] = "FIRST", "LAST: 3 failed"
	text := strings.Join(lines, "\n") + "\n"
	var tl Tool
	tl.setResult(text)
	if !tl.Truncated || !strings.HasPrefix(tl.Result, "FIRST\n") || !strings.HasSuffix(tl.ResultTail, "LAST: 3 failed\n") {
		t.Fatalf("head %q… tail …%q", tl.Result[:20], tl.ResultTail[len(tl.ResultTail)-20:])
	}
	if len(tl.Result)+len(tl.ResultTail) > maxResult {
		t.Fatalf("over the budget: %d", len(tl.Result)+len(tl.ResultTail))
	}
	// (Cut at lines both: what is left out is whole lines, counted.)
	if !strings.HasSuffix(tl.Result, "\n") || !strings.HasPrefix(text[len(text)-len(tl.ResultTail)-1:], "\n") {
		t.Fatalf("not at lines")
	}
	shown := strings.Count(tl.Result, "\n") + strings.Count(tl.ResultTail, "\n")
	if tl.Omitted != 2000-shown {
		t.Fatalf("omitted %d, want %d", tl.Omitted, 2000-shown)
	}
}

func TestSetResultOneLongLine(t *testing.T) {
	text := "Exit code 1\n" + strings.Repeat("é", 20000) + "END"
	var tl Tool
	tl.setResult(text)
	if !tl.Truncated || tl.Omitted != 0 { // (cut within a line: no count)
		t.Fatalf("%v %d", tl.Truncated, tl.Omitted)
	}
	if !strings.HasPrefix(tl.Result, "Exit code 1\n") || !strings.HasSuffix(tl.ResultTail, "END") {
		t.Fatalf("head %q tail %q", tl.Result[:12], tl.ResultTail[len(tl.ResultTail)-3:])
	}
	// (Not a head of the exit code alone: its line is short, the head goes on into the long one.)
	if len(tl.Result) < 1<<10 {
		t.Fatalf("head wasted: %d bytes", len(tl.Result))
	}
	if !utf8.ValidString(tl.Result) || !utf8.ValidString(tl.ResultTail) {
		t.Fatal("cut within a character")
	}
}

func TestSetResultNotTheBigString(t *testing.T) {
	text := strings.Repeat("a\n", 1<<20)
	var tl Tool
	tl.setResult(text)
	// (Its own copies: a slice of text would keep all of it alive.)
	if unsafe.StringData(tl.Result) == unsafe.StringData(text) {
		t.Fatal("shares the input")
	}
	if tl.Omitted == 0 || strings.Count(tl.Result, "\n")+strings.Count(tl.ResultTail, "\n")+tl.Omitted != 1<<20 {
		t.Fatalf("omitted %d", tl.Omitted)
	}
}

// The end's share begins at a line's start: nothing of it lost, the lines
// between counted.
func TestSetResultTailAtLineStart(t *testing.T) {
	tailLen := maxResult - maxHead
	end := strings.Repeat("y", 1023) + "\n" + strings.Repeat("z", tailLen-1024-1) + "\n"
	text := strings.Repeat("h\n", 3000) + end
	var tl Tool
	tl.setResult(text)
	if tl.ResultTail != end {
		t.Fatalf("tail %d bytes, want %d", len(tl.ResultTail), len(end))
	}
	if want := strings.Count(text[len(tl.Result):len(text)-len(end)], "\n"); tl.Omitted != want {
		t.Fatalf("omitted %d, want %d", tl.Omitted, want)
	}
}
