package web

// The git state of an agent's folder, for the chat page's session card: its
// branch, how far from its upstream, how much is not committed, its last
// commit, and how far it is from the remote's default branch (origin/HEAD,
// else origin/main or origin/master: commits, and the working tree's net
// change since they parted). Read only, and asked
// for only while the card is open: one short command on the host, cached a
// little while, never two at once for the same folder. The pull request of
// the branch is asked of gh apart (a network call from the host), cached
// longer.
//
// Nothing of the request goes into the command but the folder, which is the
// pane's own (from herdr or tmux, not from the page), passed as an argument.
// No remote's URL is read or answered (one may hold a token). git is kept
// from taking its optional index lock (an agent's own git command beside it
// would fail on it) and from running anything a repository's config names
// (fsmonitor, external diff, textconv).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sshx"
)

const (
	gitInfoTTL  = 10 * time.Second
	gitPRTTL    = 2 * time.Minute
	gitOutMax   = 64 << 10
	gitPaneMax  = 64
	gitWaitTime = 15 * time.Second
)

// gitInfoCommand prints the folder's git state as lines a parser reads (see
// parseGitInfo). Fixed script; the folder is its only argument. Each git
// command is bounded by the host's timeout(1) where it has one: a given-up
// command would otherwise keep its channel (and the connection's quota of
// side operations) until it ended by itself.
func gitInfoCommand(dir string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`export GIT_OPTIONAL_LOCKS=0 GIT_TERMINAL_PROMPT=0 LC_ALL=C; ` +
		`cd -- "$1" 2>/dev/null || { echo 'E nodir'; exit 0; }; ` +
		`command -v git >/dev/null 2>&1 || { echo 'E nogit'; exit 0; }; ` +
		`T=; command -v timeout >/dev/null 2>&1 && T='timeout 8'; ` +
		`g() { $T git -c core.fsmonitor=false -c core.quotepath=true "$@"; }; ` +
		`top=$(g rev-parse --show-toplevel 2>/dev/null) || { echo 'E norepo'; exit 0; }; ` +
		`printf 'TOP %s\n' "$top"; ` +
		`if st=$(g status --porcelain=v2 --branch --untracked-files=normal 2>/dev/null); then ` +
		`printf '%s\n' "$st" | awk '/^# branch\./{print; next} /^[12u] /{c++} /^\? /{u++} END{printf "N %d %d\n", c, u}'; ` +
		`else echo 'E status'; fi; ` +
		`g log -1 --no-show-signature --format='LAST %h %ct %s' 2>/dev/null; ` +
		`base=$(g symbolic-ref -q --short refs/remotes/origin/HEAD 2>/dev/null); ` +
		`[ -n "$base" ] || for b in origin/main origin/master; do g rev-parse -q --verify "refs/remotes/$b" >/dev/null 2>&1 && { base=$b; break; }; done; ` +
		`if [ -n "$base" ]; then printf 'BASE %s\n' "$base"; ` +
		`if mb=$(g merge-base HEAD "$base" 2>/dev/null); then ` +
		`printf 'BASEAHEAD %s\n' "$(g rev-list --count "$mb..HEAD" 2>/dev/null)"; ` +
		`d=$(g diff --no-ext-diff --no-textconv --shortstat "$mb" 2>/dev/null) && printf 'DIFF %s\n' "$d"; fi; fi; ` +
		`exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(dir)
}

// gitPRCommand prints, as JSON, the pull request of the folder's branch as gh
// knows it; NOGH where there is no gh, NOPR where it found none (or could not
// ask: not signed in, no network). Fixed script; the folder is its argument.
func gitPRCommand(dir string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`export GH_PROMPT_DISABLED=1 GH_NO_UPDATE_NOTIFIER=1 GIT_TERMINAL_PROMPT=0 NO_COLOR=1; ` +
		`cd -- "$1" 2>/dev/null || { echo NOPR; exit 0; }; ` +
		`command -v gh >/dev/null 2>&1 || { echo NOGH; exit 0; }; ` +
		`T=; command -v timeout >/dev/null 2>&1 && T='timeout 8'; ` +
		`$T gh pr view --json number,state,title,url,isDraft 2>/dev/null || echo NOPR; exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(dir)
}

// gitCommand is the card's one or the other: its git state, or its pull request.
func gitCommand(pr bool, dir string) string {
	if pr {
		return gitPRCommand(dir)
	}
	return gitInfoCommand(dir)
}

type gitLast struct {
	Hash    string `json:"hash"`
	Time    int64  `json:"time"`
	Subject string `json:"subject"`
}

type gitDiff struct {
	Files int `json:"files"`
	Add   int `json:"add"`
	Del   int `json:"del"`
}

type gitInfo struct {
	Repo   bool   `json:"repo"`
	Reason string `json:"reason,omitempty"` // (not a repo:) nodir, nogit, norepo
	Root   string `json:"root,omitempty"`
	// The branch; "" with Detached at a commit.
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	// Its upstream, and the commits each has the other has not ("" none).
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead,omitempty"`
	Behind   int    `json:"behind,omitempty"`
	// Files changed but not committed, and files git does not track (not
	// ignored); Status false where the status could not be read in time.
	Status    bool     `json:"status"`
	Changed   int      `json:"changed"`
	Untracked int      `json:"untracked"`
	Last      *gitLast `json:"last,omitempty"`
	// The remote's default branch, the commits since the branch left it, and
	// the working tree's net change since then (tracked files only).
	Base      string   `json:"base,omitempty"`
	BaseAhead int      `json:"base_ahead,omitempty"`
	Diff      *gitDiff `json:"diff,omitempty"`
}

// parseGitInfo reads gitInfoCommand's lines.
func parseGitInfo(out string) gitInfo {
	var g gitInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		key, rest, _ := strings.Cut(line, " ")
		switch key {
		case "E":
			if rest == "status" {
				continue // (the rest may still be read)
			}
			return gitInfo{Reason: rest}
		case "TOP":
			g.Repo, g.Root = true, rest
		case "#":
			k, v, _ := strings.Cut(rest, " ")
			switch k {
			case "branch.head":
				if v == "(detached)" {
					g.Detached = true
				} else {
					g.Branch = v
				}
			case "branch.upstream":
				g.Upstream = v
			case "branch.ab":
				a, b, _ := strings.Cut(v, " ")
				g.Ahead, _ = strconv.Atoi(strings.TrimPrefix(a, "+"))
				g.Behind, _ = strconv.Atoi(strings.TrimPrefix(b, "-"))
			}
		case "N":
			c, u, _ := strings.Cut(rest, " ")
			g.Status = true
			g.Changed, _ = strconv.Atoi(c)
			g.Untracked, _ = strconv.Atoi(u)
		case "LAST":
			f := strings.SplitN(rest, " ", 3)
			if len(f) == 3 {
				t, _ := strconv.ParseInt(f[1], 10, 64)
				g.Last = &gitLast{Hash: f[0], Time: t, Subject: f[2]}
			}
		case "BASE":
			g.Base = rest
		case "BASEAHEAD":
			g.BaseAhead, _ = strconv.Atoi(rest)
		case "DIFF":
			g.Diff = parseShortstat(rest)
		}
	}
	if !g.Repo {
		g = gitInfo{Reason: "norepo"}
	}
	return g
}

// parseShortstat reads git's "3 files changed, 10 insertions(+), 2
// deletions(-)" (each part may be missing; nothing at all is no change).
func parseShortstat(s string) *gitDiff {
	d := &gitDiff{}
	for _, part := range strings.Split(s, ",") {
		f := strings.Fields(part)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(f[1], "file"):
			d.Files = n
		case strings.HasPrefix(f[1], "insertion"):
			d.Add = n
		case strings.HasPrefix(f[1], "deletion"):
			d.Del = n
		}
	}
	return d
}

type gitPR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Draft  bool   `json:"draft"`
}

// parseGitPR reads gitPRCommand's answer: gh false where there is no gh; pr
// nil where none was found (or gh could not say).
func parseGitPR(out string) (gh bool, pr *gitPR) {
	out = strings.TrimSpace(out)
	switch out {
	case "NOGH":
		return false, nil
	case "NOPR", "":
		return true, nil
	}
	var v struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		Title   string `json:"title"`
		URL     string `json:"url"`
		IsDraft bool   `json:"isDraft"`
	}
	if json.Unmarshal([]byte(out), &v) != nil || v.Number <= 0 || !strings.HasPrefix(v.URL, "https://") || strings.ContainsAny(v.URL, " \"<>\r\n") {
		return true, nil
	}
	return true, &gitPR{Number: v.Number, State: v.State, Title: v.Title, URL: v.URL, Draft: v.IsDraft}
}

// gitCalls caches each folder's answer a while, and has a second asker of
// one under way wait for it rather than ask the host again.
type gitCalls struct {
	mu sync.Mutex
	m  map[string]*gitCall
}

type gitCall struct {
	done chan struct{}
	at   time.Time
	v    any
	err  error
}

func (c *gitCalls) do(ctx context.Context, key string, ttl time.Duration, f func() (any, error)) (any, error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*gitCall{}
	}
	if e, ok := c.m[key]; ok {
		select {
		case <-e.done:
			if e.err == nil && time.Since(e.at) < ttl {
				c.mu.Unlock()
				return e.v, nil
			}
		default:
			c.mu.Unlock()
			select {
			case <-e.done:
				return e.v, e.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	for k, x := range c.m { // (old answers go)
		select {
		case <-x.done:
			if time.Since(x.at) > 10*time.Minute {
				delete(c.m, k)
			}
		default:
		}
	}
	e := &gitCall{done: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()
	e.v, e.err = f()
	e.at = time.Now()
	close(e.done)
	if e.err != nil {
		c.mu.Lock()
		if c.m[key] == e {
			delete(c.m, key)
		}
		c.mu.Unlock()
	}
	return e.v, e.err
}

// GET /api/sessions/{id}/git[?pane=]: the folder's git state (gitInfo).
// GET /api/sessions/{id}/git/pr[?pane=]: {gh, pr} of its branch.
func (s *Server) apiGitInfo(w http.ResponseWriter, r *http.Request) { s.gitAnswer(w, r, false) }
func (s *Server) apiGitPR(w http.ResponseWriter, r *http.Request)   { s.gitAnswer(w, r, true) }

func (s *Server) gitAnswer(w http.ResponseWriter, r *http.Request, pr bool) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	pane := r.URL.Query().Get("pane")
	if len(pane) > gitPaneMax {
		writeError(w, http.StatusBadRequest, "bad_request", "no such pane")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gitWaitTime)
	defer cancel()
	cwd, err := s.paneCwd(ctx, sess, pane)
	if errors.Is(err, sshx.ErrBusy) {
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if cwd == "" {
		if pr {
			writeJSON(w, http.StatusOK, map[string]any{"gh": false, "pr": nil})
		} else {
			writeJSON(w, http.StatusOK, gitInfo{Reason: "nodir"})
		}
		return
	}
	kind, ttl := "info", gitInfoTTL
	if pr {
		kind, ttl = "pr", gitPRTTL
	}
	v, err := s.gitCalls.do(ctx, kind+"\x00"+sess.ID+"\x00"+cwd, ttl, func() (any, error) {
		out, err := sshx.Output(ctx, sess.Client(), gitCommand(pr, cwd), gitOutMax)
		if err != nil && len(out) == 0 {
			return nil, err
		}
		if pr {
			gh, p := parseGitPR(string(out))
			return map[string]any{"gh": gh, "pr": p}, nil
		}
		return parseGitInfo(string(out)), nil
	})
	switch {
	case errors.Is(err, sshx.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case err != nil:
		writeError(w, http.StatusBadGateway, "host_error", "the host did not say")
	default:
		writeJSON(w, http.StatusOK, v)
	}
}
