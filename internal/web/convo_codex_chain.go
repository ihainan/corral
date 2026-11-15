package web

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// A Codex conversation rewound (Esc Esc, then a message: 0.167, real, 11-15)
// goes on in a log of its own, rollout-<time>-<thread>_<writer>.jsonl, that
// holds only what came after: its first line (session_meta) says what it
// goes on from — history_mode "paginated", history_base {thread_id: the
// writer of the log before (the thread's own id for its first log),
// end_byte_offset: how much of that log comes before, end_ordinal_exclusive:
// the ordinal its rows go on at}. Rewound again, the same, from that one.
// The whole conversation is each log before up to where the next goes on
// from it, then the last; what was rewound stays in the log before, past
// that point, and is not read. (Its database names the last.)

// codexBase is where a Codex log goes on from.
type codexBase struct {
	writer  string // the log before's writer: the thread's id for its first log
	end     int64  // how much of the log before comes first (bytes)
	ordinal int64  // the ordinal the log's own rows go on at
}

// codexHistoryBase reads a Codex log's first line (head: the log's start,
// that line whole in it): where it goes on from, if it does.
func codexHistoryBase(head []byte) (codexBase, bool) {
	line, _, found := bytes.Cut(head, []byte("\n"))
	if !found {
		return codexBase{}, false
	}
	var m struct {
		Type    string `json:"type"`
		Payload struct {
			Mode string `json:"history_mode"`
			Base *struct {
				Thread  string `json:"thread_id"`
				End     int64  `json:"end_byte_offset"`
				Ordinal int64  `json:"end_ordinal_exclusive"`
			} `json:"history_base"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "session_meta" || m.Payload.Base == nil {
		return codexBase{}, false
	}
	b := m.Payload.Base
	if !convoUUID.MatchString(b.Thread) || b.End <= 0 || b.Ordinal <= 0 {
		return codexBase{}, false
	}
	return codexBase{writer: strings.ToLower(b.Thread), end: b.End, ordinal: b.Ordinal}, true
}

// codexSegmentName: a Codex log's name — rollout-<time>-<thread>.jsonl, or
// a segment of it after a rewind, rollout-<time>-<thread>_<writer>.jsonl:
// its thread and writer ("" for the first).
var codexSegmentName = regexp.MustCompile(`^rollout-[^/_]+-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?:_([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}))?\.jsonl$`)

// dirLister lists a folder (sftplite.Client's ReadDirLimit).
type dirLister interface {
	ReadDirLimit(p string, max int) ([]os.FileInfo, bool, error)
}

// codexBaseDirMax: the most entries of a day's folder looked through.
const codexBaseDirMax = 20000

// codexBaseLog finds the log a Codex log (log, of thread) goes on from,
// written by writer: in its own day's folder, else in that of the day the
// writer's id was made (a UUIDv7's time), or the day either side (the
// folder is named by the host's local day). "" where none, or more than one.
func codexBaseLog(c dirLister, log, thread, writer string) string {
	dir := path.Dir(log)
	root := path.Dir(path.Dir(path.Dir(dir))) // (…/sessions)
	dirs := []string{dir}
	if t, ok := uuidV7Time(writer); ok {
		for _, d := range []int{0, -1, 1} {
			day := t.AddDate(0, 0, d)
			dirs = append(dirs, path.Join(root, day.Format("2006"), day.Format("01"), day.Format("02")))
		}
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		entries, _, err := c.ReadDirLimit(d, codexBaseDirMax)
		if err != nil {
			continue
		}
		found := ""
		for _, e := range entries {
			m := codexSegmentName.FindStringSubmatch(e.Name())
			if m == nil || !e.Mode().IsRegular() || !strings.EqualFold(m[1], thread) {
				continue
			}
			if (writer == strings.ToLower(thread) && m[2] == "") || strings.EqualFold(m[2], writer) {
				if found != "" {
					return "" // (two alike: not guessed between)
				}
				found = path.Join(d, e.Name())
			}
		}
		if found != "" {
			return found
		}
	}
	return ""
}

// uuidV7Time: when a version 7 UUID was made (its first 48 bits, unix ms).
func uuidV7Time(id string) (time.Time, bool) {
	h := strings.ReplaceAll(id, "-", "")
	if len(h) != 32 || h[12] != '7' {
		return time.Time{}, false
	}
	b, err := hex.DecodeString(h[:12])
	if err != nil {
		return time.Time{}, false
	}
	var ms int64
	for _, x := range b {
		ms = ms<<8 | int64(x)
	}
	return time.UnixMilli(ms), true
}

// codexBaseEndsRight: what was read of the log before, up to where the
// next goes on from it (part: its end there), ends as said — a whole line
// there, its ordinal the one before the next's first.
func codexBaseEndsRight(part []byte, b codexBase) bool {
	if len(part) == 0 || part[len(part)-1] != '\n' {
		return false
	}
	body := part[:len(part)-1]
	last := body[bytes.LastIndexByte(body, '\n')+1:]
	var r struct {
		Ordinal *int64 `json:"ordinal"`
	}
	if json.Unmarshal(last, &r) != nil || r.Ordinal == nil {
		return false
	}
	return *r.Ordinal == b.ordinal-1
}

// codexHopsMax: the most logs before the last one read through.
const codexHopsMax = 32
