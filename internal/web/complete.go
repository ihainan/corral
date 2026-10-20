package web

// Completion for the composer: after "@" the files under the folder the
// terminal is in, after "/" and "$" the commands and skills of the coding
// agents found on the host (the agents' built-in commands are known to the
// browser).

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sshx"
)

const (
	completeFilesMax = 5000             // paths listed at most
	completeTTL      = 30 * time.Second // a listing is reused this long
	completeOutMax   = 1 << 20
)

// completeCommand lists, from the folder given as its argument (else the
// home folder), as NUL-terminated records (no name can fake one): the
// folder; the agents' own commands and skills ("claude /name", "codex
// /prompts:name", "codex $name": Claude Code's in the home folder and the
// project, Codex's prompts and skills); a marker record; the files there
// (git's, else a bounded walk that skips hidden and dependency folders and
// files). Fixed script; the folder is its only argument.
func completeCommand(dir string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`if [ -n "$1" ]; then cd -- "$1" 2>/dev/null || exit 3; else cd || exit 3; fi; printf '%s\0' "$(pwd)"; ` +
		`for f in "$HOME"/.claude/commands/*.md .claude/commands/*.md; do [ -f "$f" ] && { b=${f##*/}; printf 'claude /%s\0' "${b%.md}"; }; done; ` +
		`for d in "$HOME"/.claude/skills/*/ .claude/skills/*/; do [ -f "$d/SKILL.md" ] && { d=${d%/}; printf 'claude /%s\0' "${d##*/}"; }; done; ` +
		`for f in "$HOME"/.codex/prompts/*.md; do [ -f "$f" ] && { b=${f##*/}; printf 'codex /prompts:%s\0' "${b%.md}"; }; done; ` +
		`for d in "$HOME"/.codex/skills/*/ "$HOME"/.agents/skills/*/ .agents/skills/*/; do [ -f "$d/SKILL.md" ] && { d=${d%/}; printf 'codex $%s\0' "${d##*/}"; }; done; ` +
		`printf '%s\0' '` + completeMarker + `'; ` +
		`git ls-files -z -co --exclude-standard 2>/dev/null || ` +
		`find . -maxdepth 6 \( -name '.?*' -o -name node_modules -o -name venv -o -name __pycache__ -o -name target -o -name dist \) -prune -o -type f -print0 2>/dev/null; ` +
		`exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(dir)
}

// completeMarker ends the commands (no command record is just this).
const completeMarker = "-- corral files --"

// A command or skill as offered: "/name", "/prompts:name" or "$name".
var completeName = regexp.MustCompile(`^[/$][A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

const (
	completePathMax  = 512 // bytes of a path offered
	completeDepthMax = 16  // folders deep
)

type completeCmd struct {
	Agent string `json:"agent"` // claude or codex
	Name  string `json:"name"`
}

type completeList struct {
	Cwd      string        `json:"cwd"` // the folder the files are in
	Files    []string      `json:"files"`
	Commands []completeCmd `json:"commands"`
	More     bool          `json:"more"` // the folder has more files than listed
}

// parseComplete reads completeCommand's output (cut short by the output
// limit, the last file is incomplete and left out). Paths that could not be
// typed into a prompt as they are (control characters, absolute, going up,
// too long or deep) are left out.
func parseComplete(out string) completeList {
	l := completeList{Files: []string{}, Commands: []completeCmd{}}
	recs := strings.Split(out, "\x00")
	if last := recs[len(recs)-1]; last != "" {
		l.More = true // (cut short)
	}
	recs = recs[:len(recs)-1]
	if len(recs) == 0 {
		return l
	}
	l.Cwd = recs[0]
	i := 1
	for ; i < len(recs) && recs[i] != completeMarker; i++ {
		agent, name, ok := strings.Cut(recs[i], " ")
		if ok && (agent == "claude" || agent == "codex") && completeName.MatchString(name) {
			l.Commands = append(l.Commands, completeCmd{Agent: agent, Name: name})
		}
	}
	for _, s := range recs[min(i+1, len(recs)):] {
		s = strings.TrimPrefix(s, "./")
		if len(l.Files) == completeFilesMax {
			l.More = true
			break
		}
		if s == "" || len(s) > completePathMax || strings.Count(s, "/") > completeDepthMax || strings.HasPrefix(s, "/") || s == ".." || strings.HasPrefix(s, "../") ||
			strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) || !utf8.ValidString(s) {
			continue
		}
		l.Files = append(l.Files, s)
	}
	return l
}

// completions caches listings per session and folder.
type completions struct {
	mu sync.Mutex
	m  map[string]completeEntry
}

type completeEntry struct {
	at   time.Time
	list completeList
}

func (c *completions) get(key string) (completeList, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) > completeTTL {
		return completeList{}, false
	}
	return e.list, true
}

func (c *completions) put(key string, l completeList) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]completeEntry{}
	}
	for k, e := range c.m {
		if time.Since(e.at) > completeTTL {
			delete(c.m, k)
		}
	}
	c.m[key] = completeEntry{at: time.Now(), list: l}
}

// GET /api/sessions/{id}/complete
func (s *Server) apiComplete(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cwd, err := s.sessionCwd(ctx, sess)
	if errors.Is(err, sshx.ErrBusy) {
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
		return
	}
	key := sess.ID + "\x00" + cwd
	if l, ok := s.complete.get(key); ok {
		writeJSON(w, http.StatusOK, l)
		return
	}
	out, err := sshx.Output(ctx, sess.Client(), completeCommand(cwd), completeOutMax)
	switch {
	case errors.Is(err, sshx.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
		return
	case err != nil && len(out) == 0:
		writeError(w, http.StatusBadGateway, "host_error", "the host did not list the files")
		return
	}
	l := parseComplete(string(out))
	s.complete.put(key, l)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, l)
}
