package web

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A real Codex 0.167.2's logs of one conversation rewound twice (the test
// container, a mock model, 11-15): "one", "two"; rewound to before "two",
// "three", "four"; rewound to before "four", "five".
const (
	rwThread = "01a1dedc-8c62-79e2-a0b5-59371b5979d7"
	rwFirst  = "testdata/codex-rewind/rollout-2026-11-15T10-59-18-01a1dedc-8c62-79e2-a0b5-59371b5979d7.jsonl"
	rwSeg1   = "testdata/codex-rewind/rollout-2026-11-15T10-59-35-01a1dedc-8c62-79e2-a0b5-59371b5979d7_01a1dedc-d19f-7b22-8b91-b1f46090634d.jsonl"
	rwSeg2   = "testdata/codex-rewind/rollout-2026-11-15T10-59-49-01a1dedc-8c62-79e2-a0b5-59371b5979d7_01a1dedd-06de-7860-87ed-b854389caf1f.jsonl"
)

func readFile(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCodexHistoryBase(t *testing.T) {
	if _, ok := codexHistoryBase(readFile(t, rwFirst)); ok {
		t.Error("the first log goes on from another")
	}
	b, ok := codexHistoryBase(readFile(t, rwSeg1))
	if !ok || b.writer != rwThread || b.end != 97254 || b.ordinal != 19 {
		t.Fatalf("segment 1: %+v %v", b, ok)
	}
	b2, ok := codexHistoryBase(readFile(t, rwSeg2))
	if !ok || b2.writer != "01a1dedc-d19f-7b22-8b91-b1f46090634d" || b2.end != 35255 || b2.ordinal != 32 {
		t.Fatalf("segment 2: %+v %v", b2, ok)
	}
	// (Its end where the next goes on: a whole line, the ordinal before.)
	first, seg1 := readFile(t, rwFirst), readFile(t, rwSeg1)
	if !codexBaseEndsRight(first[:b.end], b) || !codexBaseEndsRight(seg1[:b2.end], b2) {
		t.Error("a real cut not taken")
	}
	if codexBaseEndsRight(first[:b.end-1], b) || codexBaseEndsRight(first[:b.end], b2) || codexBaseEndsRight(seg1[:b2.end], b) {
		t.Error("a wrong cut taken")
	}
	if _, ok := codexHistoryBase([]byte(`{"type":"session_meta","payload":{"history_base":{"thread_id":"x; rm","end_byte_offset":5,"end_ordinal_exclusive":2}}}` + "\n")); ok {
		t.Error("not an id, taken")
	}
	if _, ok := codexHistoryBase([]byte(`{"type":"session_meta","payload":{"history_base":{"thread_id":"` + rwThread + `"}}}`)); ok {
		t.Error("its first line not whole, taken")
	}
}

type fakeDir map[string][]os.FileInfo

func (f fakeDir) ReadDirLimit(p string, _ int) ([]os.FileInfo, bool, error) {
	e, ok := f[p]
	if !ok {
		return nil, false, os.ErrNotExist
	}
	return e, false, nil
}

type brokenDir struct{}

func (brokenDir) ReadDirLimit(string, int) ([]os.FileInfo, bool, error) {
	return nil, false, errors.New("connection lost")
}

func infos(t *testing.T, names ...string) []os.FileInfo {
	dir := t.TempDir()
	var out []os.FileInfo
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fi)
	}
	return out
}

// The log before: by its writer, in the same day's folder, else in that of
// the day its writer was made (or either side); two alike: none.
func TestCodexBaseLog(t *testing.T) {
	first, seg1, seg2 := filepath.Base(rwFirst), filepath.Base(rwSeg1), filepath.Base(rwSeg2)
	const day = "/h/.codex/sessions/2026/11/15"
	live := day + "/" + seg2
	d := fakeDir{day: infos(t, first, seg1, seg2, "rollout-2026-11-15T10-00-00-01a1dedc-0000-79e2-a0b5-59371b5979d7.jsonl")}
	if got, _ := codexBaseLog(d, live, rwThread, "01a1dedc-d19f-7b22-8b91-b1f46090634d"); got != day+"/"+seg1 {
		t.Errorf("segment 1: %q", got)
	}
	if got, _ := codexBaseLog(d, day+"/"+seg1, rwThread, rwThread); got != day+"/"+first {
		t.Errorf("the first: %q", got)
	}
	if got, _ := codexBaseLog(d, live, rwThread, "01a1dedc-ffff-7b22-8b91-b1f46090634d"); got != "" {
		t.Errorf("a writer of none: %q", got)
	}
	// (The day before, past midnight: by its writer's time.)
	at, _ := uuidV7Time(rwThread)
	prev := "/h/.codex/sessions/" + at.Format("2006/01/02")
	moved := fakeDir{day: infos(t, seg1, seg2), prev: infos(t, first)}
	if got, _ := codexBaseLog(moved, day+"/"+seg1, rwThread, rwThread); got != prev+"/"+first {
		t.Errorf("another day's: %q", got)
	}
	twice := fakeDir{day: infos(t, seg1, seg2, first, "rollout-2026-11-15T11-00-00-01a1dedc-8c62-79e2-a0b5-59371b5979d7.jsonl")}
	if got, _ := codexBaseLog(twice, day+"/"+seg1, rwThread, rwThread); got != "" {
		t.Errorf("two alike: %q", got)
	}
}

// A folder not read (the host, not a missing folder): an error, tried again
// later — not "none before" for good (review r2b-5).
func TestCodexBaseLogNotRead(t *testing.T) {
	if got, err := codexBaseLog(brokenDir{}, "/h/.codex/sessions/2026/11/15/"+filepath.Base(rwSeg1), rwThread, rwThread); got != "" || err == nil {
		t.Errorf("%q %v", got, err)
	}
}

func TestUUIDV7Time(t *testing.T) {
	at, ok := uuidV7Time(rwThread)
	if !ok || at.UTC().Format(time.RFC3339) != "2026-11-15T10:59:18Z" {
		t.Errorf("%v %v", at.UTC(), ok)
	}
	if _, ok := uuidV7Time("0f9a2c1e-1111-4222-8333-944455556666"); ok {
		t.Error("a v4 taken")
	}
}

// A copy ("codex fork", 0.167): its first line names the original's log by
// its writer — the original's own id for its first log, a segment's id for
// one rewound — a log of another thread, found all the same.
func TestCodexBaseLogFork(t *testing.T) {
	const day = "/h/.codex/sessions/2026/11/15"
	const src = "01a1dff5-1931-73c3-9a56-5747dad23014"
	const copyID = "01a1dff5-50a7-7130-b294-d74e463261b7"
	const seg = "01a1dff5-3333-7b22-8b91-b1f46090634d"
	srcFirst := "rollout-2026-11-15T16-05-40-" + src + ".jsonl"
	srcSeg := "rollout-2026-11-15T16-05-50-" + src + "_" + seg + ".jsonl"
	copyLog := "rollout-2026-11-15T16-05-58-" + copyID + ".jsonl"
	d := fakeDir{day: infos(t, srcFirst, srcSeg, copyLog)}
	if got, _ := codexBaseLog(d, day+"/"+copyLog, copyID, src); got != day+"/"+srcFirst {
		t.Errorf("the original's first log: %q", got)
	}
	if got, _ := codexBaseLog(d, day+"/"+copyLog, copyID, seg); got != day+"/"+srcSeg {
		t.Errorf("the original's segment: %q", got)
	}
}
