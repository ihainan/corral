package web

import (
	"regexp"
	"strconv"
	"strings"
)

// What a Codex at work says only on its screen (its log says nothing of it
// till it is over, 11-15 parity audit): its request failing and tried again
// ("• Reconnecting... 2/5 (7s • esc to interrupt)", the reason on the line
// under it: "└ Exceeded retry limit, last status: 429 Too Many Requests"),
// or compacting the conversation ("• Compacting context (4s • esc to
// interrupt)"). Only its status line now counts: the last such line, over
// its input box (the same words higher up are the conversation's).

// codexStatusLine: Codex's line of what it does now, its mark blinking
// ("•"/"◦"), its time, and what else goes on after it (" · 1 background
// terminal running · …").
var codexStatusLine = regexp.MustCompile(`^[•◦] (\S.*?) \((?:\d+[hm] )*\d+s • esc to interrupt\)(?: · .*)?$`)

var codexRetrying = regexp.MustCompile(`^Reconnecting\.\.\. (\d{1,3})/(\d{1,3})$`)

type codexRetry struct {
	N   int    `json:"n"`
	Of  int    `json:"of"`
	Why string `json:"why,omitempty"`
}

// codexWorkNote: what a Codex's screen (without its colours) says it does
// that its log does not: compacting, or a request tried again (n of m, and
// why); neither where its status line says other.
func codexWorkNote(screen string) (compacting bool, retry *codexRetry) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		m := codexStatusLine.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		what := m[1]
		if strings.HasPrefix(what, "Compacting context") {
			return true, nil
		}
		if r := codexRetrying.FindStringSubmatch(what); r != nil {
			n, _ := strconv.Atoi(r[1])
			of, _ := strconv.Atoi(r[2])
			if n < 1 || of < n {
				return false, nil
			}
			why := ""
			if i+1 < len(lines) {
				if w, ok := strings.CutPrefix(strings.TrimSpace(lines[i+1]), "└ "); ok {
					why = clipRunes(strings.TrimSpace(w), 200)
				}
			}
			return false, &codexRetry{N: n, Of: of, Why: why}
		}
		return false, nil
	}
	return false, nil
}
