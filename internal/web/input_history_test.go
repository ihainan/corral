package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func histLine(display string) string {
	b, _ := json.Marshal(map[string]any{"display": display, "timestamp": 1, "project": "/p"})
	return string(b) + "\n"
}

// histAll pages through file as the browser would (each page a request:
// its own scan budget), q lower case; it gives the displays found, latest
// first, and the lines skipped.
func histAll(t *testing.T, file string, q string) ([]string, int) {
	t.Helper()
	read := func(off int64, n int) ([]byte, error) {
		if off < 0 || off+int64(n) > int64(len(file)) {
			return []byte(file[min(int(off), len(file)):]), nil
		}
		return []byte(file[off : off+int64(n)]), nil
	}
	st := histState{Cursor: int64(len(file))}
	if len(file) > 0 && file[len(file)-1] != '\n' {
		st.Skip = "tail"
	}
	var got []string
	skipped := 0
	for pages := 0; ; pages++ {
		if pages > 10000 {
			t.Fatal("no end")
		}
		var last *histPage
		err := histScan(read, st, q, func() bool { return false }, func(pg histPage, blk []byte, at int64) {
			if blk != nil && (at < 0 || at+int64(len(blk)) > int64(len(file)) || string(blk) != file[at:at+int64(len(blk))]) {
				t.Fatal("the block given is not the file's")
			}
			pg.Items = append([]histItem(nil), pg.Items...)
			last = &pg
		})
		if err != nil {
			t.Fatal(err)
		}
		if last == nil {
			t.Fatalf("a page with no point past %+v", st)
		}
		if last.At == st && len(last.Items) == 0 && !last.Done && last.Skipped == 0 {
			t.Fatalf("stuck at %+v", st)
		}
		for _, it := range last.Items {
			if it.TextCut {
				got = append(got, "CUT:"+it.Preview)
			} else {
				got = append(got, it.Text)
			}
		}
		skipped += last.Skipped
		if last.Done {
			return got, skipped
		}
		if last.At.Skip == "" && (last.At.Cursor == 0 || file[last.At.Cursor-1] != '\n') {
			t.Fatalf("a cursor not at a line's start: %d", last.At.Cursor)
		}
		st = last.At
	}
}

func reversed(s []string) []string {
	out := append([]string(nil), s...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func sameList(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d items, want %d", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: item %d %.40q, want %.40q", what, i, got[i], want[i])
		}
	}
}

// Many lines of many lengths across blocks and requests: every one, once,
// in order; none lost at a block's front.
func TestHistScanWhole(t *testing.T) {
	var file strings.Builder
	var want []string
	for i := 0; i < 3000; i++ {
		d := fmt.Sprintf("m%d %s", i, strings.Repeat("x", (i*7919)%3000))
		want = append(want, d)
		file.WriteString(histLine(d))
	}
	got, skipped := histAll(t, file.String(), "")
	sameList(t, "whole", got, reversed(want))
	if skipped != 0 {
		t.Fatalf("skipped %d", skipped)
	}
	// Few found: pages go across many blocks and their fronts.
	file.Reset()
	var found []string
	for i := 0; i < 3000; i++ {
		d := fmt.Sprintf("m%d %s", i, strings.Repeat("x", (i*7919)%3000))
		if i%97 == 0 {
			d += " Needle"
			found = append(found, d)
		}
		file.WriteString(histLine(d))
	}
	got, _ = histAll(t, file.String(), "needle")
	sameList(t, "sparse", got, reversed(found))
}

// A line of just a block (its newline too) is read, not skipped; one
// longer is skipped and counted once, however many requests finding its
// start takes; a file's unfinished end is passed over, not counted.
func TestHistScanLongLines(t *testing.T) {
	exact := ""
	for n := histBlock - 200; ; n++ {
		if l := histLine(strings.Repeat("e", n)); len(l) == histBlock {
			exact = l
			break
		}
	}
	var file strings.Builder
	file.WriteString(histLine("first"))
	file.WriteString(exact)
	file.WriteString(histLine("after exact"))
	file.WriteString(histLine(strings.Repeat("L", histBlock+10)))
	file.WriteString(histLine("after long"))
	file.WriteString(histLine(strings.Repeat("H", 3*histScanMax))) // (longer than three requests' reading)
	file.WriteString(histLine("after huge"))
	whole := file.String()
	got, skipped := histAll(t, whole, "")
	sameList(t, "long", got, []string{"after huge", "after long", "after exact", "CUT:" + clipRunes(strings.Repeat("e", len(exact)-len(histLine(""))), histPreview), "first"})
	if skipped != 2 {
		t.Fatalf("skipped %d, want 2", skipped)
	}
	// An end being written, longer than a request reads: passed over, not counted.
	got, skipped = histAll(t, whole+`{"display":"half`+strings.Repeat("t", 2*histScanMax), "")
	if len(got) != 5 || got[0] != "after huge" || skipped != 2 {
		t.Fatalf("an unfinished end: %d items (%.20q), skipped %d", len(got), got[0], skipped)
	}
	// The file's first line longer than a block: read to the start, counted.
	got, skipped = histAll(t, histLine(strings.Repeat("F", histBlock+5))+histLine("second"), "")
	if len(got) != 1 || got[0] != "second" || skipped != 1 {
		t.Fatalf("a long first line: %v %d", got, skipped)
	}
}

// Full pages (by count, or by bytes) end between lines: the next begins at
// the line not taken.
func TestHistScanFullPages(t *testing.T) {
	var file strings.Builder
	var want []string
	for i := 0; i < 130; i++ {
		d := fmt.Sprintf("short %d", i)
		want = append(want, d)
		file.WriteString(histLine(d))
	}
	got, _ := histAll(t, file.String(), "")
	sameList(t, "by count", got, reversed(want))
	file.Reset()
	want = nil
	for i := 0; i < 60; i++ {
		d := fmt.Sprintf("big %d %s", i, strings.Repeat("b", histTextMax-20))
		want = append(want, d)
		file.WriteString(histLine(d))
	}
	got, _ = histAll(t, file.String(), "")
	sameList(t, "by bytes", got, reversed(want))
}

// Searched as read: only what holds the words, whatever their case.
func TestHistScanSearch(t *testing.T) {
	file := histLine("Fix the LOGIN bug") + histLine("other") + histLine("登录 login 页面") + histLine("[Pasted text #1 +20 lines] see")
	got, _ := histAll(t, file, "login")
	sameList(t, "search", got, []string{"登录 login 页面", "Fix the LOGIN bug"})
	it, ok := histItemOf([]byte(strings.TrimSpace(histLine("[Pasted text #1 +20 lines] see"))), "")
	if !ok || !it.Pasted {
		t.Fatalf("pasted: %+v", it)
	}
}

// Another file under the cursor (shorter, or the cursor no line's start): said so.
func TestHistScanChanged(t *testing.T) {
	file := histLine("a") + histLine("b")
	read := func(off int64, n int) ([]byte, error) {
		end := min(off+int64(n), int64(len(file)))
		return []byte(file[off:end]), nil
	}
	if err := histScan(read, histState{Cursor: int64(len(file)) + 10}, "", func() bool { return false }, func(histPage, []byte, int64) {}); err != errHistChanged {
		t.Fatalf("shorter: %v", err)
	}
	if err := histScan(read, histState{Cursor: 3}, "", func() bool { return false }, func(histPage, []byte, int64) {}); err != errHistChanged {
		t.Fatalf("not a line's start: %v", err)
	}
}

// Stopped before reading: no point past where it began.
func TestHistScanStopped(t *testing.T) {
	file := histLine("a")
	n := 0
	err := histScan(func(off int64, k int) ([]byte, error) { return []byte(file[off : off+int64(k)]), nil }, histState{Cursor: int64(len(file))}, "", func() bool { return true }, func(histPage, []byte, int64) { n++ })
	if err != nil || n != 0 {
		t.Fatalf("stopped: %v %d", err, n)
	}
}
