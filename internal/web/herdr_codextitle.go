package web

import (
	"context"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// A Codex pane's name for what it does. Codex titles its terminal
// "<thread> | <project>" (its default [tui].terminal_title), or only the
// project while the thread has no name yet — and a thread resumed from one
// imported (from Claude Code, say) never gets one. Shown as it is, every
// such pane would read as its folder ("AI", "AI", "AI"). So the gateway
// takes the thread's name out of the title, and where the title has none,
// asks Codex's own database for the thread's name, else its title, else
// its first message: the thread being the one the pane's Codex resumed
// (`codex resume <id>`) or the log it has open. Looked for only while such
// a pane is there, at most every codexTitleEvery; what is found stands in
// for what was.

const (
	codexTitleEvery = 2 * time.Minute
	codexTitleSoon  = 2 * time.Second
	codexNameRunes  = 80
)

// codexTitleParts reads a Codex pane's terminal title (agent: herdr's
// name for its program): the thread's name in it, or none and bare — it
// names only where the agent runs (or nothing). Not a Codex's: none, not
// bare.
func codexTitleParts(agent, title, cwd string) (thread string, bare bool) {
	if !isCodexAgent(agent) {
		return "", false
	}
	t := strings.TrimLeftFunc(title, func(r rune) bool {
		return unicode.IsSpace(r) || r >= 0x2800 && r <= 0x28ff || strings.ContainsRune("·✢✳✶✻✽*◐◑◒◓○●◌", r) // (a spinner Codex may put before it)
	})
	t = strings.TrimSpace(t)
	project := ""
	if p := strings.TrimRight(cwd, "/"); p != "" && !homeDir.MatchString(p) {
		project = path.Base(p)
	}
	isProject := func(s string) bool {
		if project == "" || s == "" {
			return false
		}
		if s == project {
			return true
		}
		// (Codex cuts a long one short: "ZGCAI-Agent-Platform-...".)
		for _, cut := range []string{"...", "…"} {
			if head, ok := strings.CutSuffix(s, cut); ok && head != "" && strings.HasPrefix(project, head) {
				return true
			}
		}
		return false
	}
	if t == "" || isProject(t) {
		return "", true
	}
	if i := strings.LastIndex(t, " | "); i > 0 && isProject(strings.TrimSpace(t[i+3:])) {
		if name := strings.TrimSpace(t[:i]); name != "" {
			return name, false
		}
		return "", true
	}
	return "", false
}

var homeDir = regexp.MustCompile(`^(?:/home/[^/]+|/Users/[^/]+|/root)$`)

func isCodexAgent(agent string) bool { return strings.Contains(strings.ToLower(agent), "codex") }

// codexName: the name a thread is shown by, of what Codex's database has
// of it — its name, else its title, else its first message: the first line
// with words in it, without the placeholders of images and the like, cut
// short. "" where none has any.
func codexName(name, title, first string) string {
	for _, s := range []string{name, title, first} {
		if c := cleanCodexName(s); c != "" {
			return c
		}
	}
	return ""
}

var codexPlaceholder = regexp.MustCompile(`\[(?:Image #\d+|external [^\]\n]{0,80}|Pasted text[^\]\n]{0,40})\]`)

func cleanCodexName(s string) string {
	s = codexPlaceholder.ReplaceAllString(s, " ")
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.FieldsFunc(line, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > codexNameRunes {
			r := []rune(line)
			line = strings.TrimSpace(string(r[:codexNameRunes-1])) + "…"
		}
		return line
	}
	return ""
}

// codexTitler finds, for a herdr server's Codex panes whose title is bare,
// their thread's name (see above).
type codexTitler struct {
	mu    sync.Mutex
	want  map[string]bool   // panes whose title is bare, as the last summary had them
	found map[string]string // pane → its thread's name, as last found
	soon  bool              // one wanted since the last look
	wake  chan struct{}
	poke  func()
	scan  func(ctx context.Context) (map[string]string, error)
	every time.Duration
}

func newCodexTitler(scan func(context.Context) (map[string]string, error)) *codexTitler {
	return &codexTitler{want: map[string]bool{}, found: map[string]string{}, wake: make(chan struct{}, 1), scan: scan, every: codexTitleEvery}
}

func (c *codexTitler) setPoke(f func()) {
	c.mu.Lock()
	c.poke = f
	c.mu.Unlock()
}

// track takes the panes to look for from a summary (all of them: the rest
// are not looked for, and what was found of them is let go).
func (c *codexTitler) track(sum *herdrSummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := map[string]bool{}
	for _, w := range sum.Workspaces {
		for _, a := range w.Agents {
			if a.BareTitle {
				want[a.PaneID] = true
				if !c.want[a.PaneID] {
					c.soon = true
				}
			}
		}
	}
	for p := range c.found {
		if !want[p] {
			delete(c.found, p)
		}
	}
	c.want = want
	if c.soon {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// decorate gives each Codex pane whose title is bare its thread's name,
// where it was found.
func (c *codexTitler) decorate(sum *herdrSummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range sum.Workspaces {
		for j := range sum.Workspaces[i].Agents {
			a := &sum.Workspaces[i].Agents[j]
			if a.BareTitle && a.ThreadTitle == "" {
				a.ThreadTitle = c.found[a.PaneID]
			}
		}
	}
}

// run looks when due, until ctx ends: soon after a pane comes to be
// wanted, then every c.every while any is.
func (c *codexTitler) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var last time.Time
	for {
		c.mu.Lock()
		wait := time.Hour
		if len(c.want) > 0 {
			wait = max(0, time.Until(last.Add(c.every)))
			if c.soon {
				wait = min(wait, max(0, time.Until(last.Add(codexTitleSoon))))
			}
		}
		c.mu.Unlock()
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			continue
		case <-timer.C:
		}
		c.mu.Lock()
		due := len(c.want) > 0
		c.soon = false
		c.mu.Unlock()
		if !due {
			continue
		}
		last = time.Now()
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		got, err := c.scan(sctx)
		cancel()
		if err != nil {
			continue // (what was found stands; looked for again in its time)
		}
		c.mu.Lock()
		changed := false
		for p := range c.want {
			if got[p] != c.found[p] {
				changed = true
				if got[p] == "" {
					delete(c.found, p)
				} else {
					c.found[p] = got[p]
				}
			}
		}
		poke := c.poke
		c.mu.Unlock()
		if changed && poke != nil {
			poke()
		}
	}
}
