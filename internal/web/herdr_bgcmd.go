package web

import (
	"context"
	"github.com/ihainan/corral/internal/term"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
)

// Commands a Claude Code agent left running in the background (plans/4e):
// what it ran with run_in_background, or was sent there (Ctrl+B), and is
// still at it. Claude Code runs each in a shell of its own whose output
// goes to <tmp>/claude-<uid>/<folder>/<session>/tasks/<task>.output, so
// they are read off the host's processes, not the log: one is running for
// as long as such a process of a Claude Code is. A Monitor's output comes
// through a pipe, not a file: one is told by its shell (Claude Code's own,
// running the command by eval), its session by the CLAUDE_CODE_SESSION_ID
// Claude Code gave it — the only one of its environment read.
//
// Looked at apart from the subagents (bgProber): a look that fails says
// nothing of these, and changes nothing there, nor any "finished" alert.
// Looked at only while a Claude Code pane is done or idle: once a minute,
// and a few seconds after one comes to it (what it set going as its turn
// ended may only just have begun). One look tells of every session on the
// host; what it found stands in for what was found before.

// cmdEvery: how often it is looked at while a Claude pane is done or idle;
// cmdSoon: how soon after one comes to that; cmdKept: how long what a look
// found is told, none since. (A prober's own: the tests set theirs.)
const (
	cmdEvery = time.Minute
	cmdSoon  = 5 * time.Second
	cmdKept  = 3 * time.Minute
)

const (
	maxCmdsShown = 8 // listed per agent (all are counted; the card lists each, 11-15)
	cmdScanMax   = 128 << 10
)

// bgCmd is a command running in the background: its task, what it runs
// (as far as it could be told), and when it began (unix ms).
type bgCmd struct {
	Task  string `json:"task"`
	Title string `json:"title,omitempty"`
	Since int64  `json:"since,omitempty"`
	// Out: the file its output goes to (none for a Monitor's), for the page
	// to show its end, as of one the log told of.
	Out string `json:"out,omitempty"`
}

type cmdProber struct {
	mu    sync.Mutex
	ended map[string]cmdPane // the Claude panes done or idle
	cmds  map[string][]bgCmd // agent session → its commands, as last found
	at    time.Time          // when they were found (zero: not known)
	soon  time.Time          // a look due soon (a pane came to done or idle)
	wake  chan struct{}
	poke  func()
	scan  func(ctx context.Context) (cmds map[string][]bgCmd, whole bool, err error)

	every, soonBy, kept time.Duration // (cmdEvery, cmdSoon, cmdKept)
	// forget: a pane not considered again for this long is no longer looked
	// for (0: kept till it is said to work again). For panes whose status is
	// told only while someone reads them (a tmux session's chat): none read,
	// no looks.
	forget time.Duration
}

type cmdPane struct {
	session string
	from    time.Time // when it came to done or idle: what a look begun before found is not told of it
	told    time.Time // when it was last considered (see forget)
}

func newCmdProber(scan func(context.Context) (map[string][]bgCmd, bool, error)) *cmdProber {
	return &cmdProber{ended: map[string]cmdPane{}, wake: make(chan struct{}, 1), scan: scan, every: cmdEvery, soonBy: cmdSoon, kept: cmdKept}
}

func (p *cmdProber) setPoke(f func()) {
	p.mu.Lock()
	p.poke = f
	p.mu.Unlock()
}

// consider: pane's agent is in status, of agent session session.
func (p *cmdProber) consider(pane, agent, status, session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ended := status == herdr.StatusDone || status == herdr.StatusIdle
	if a := strings.ToLower(agent); !ended || !(strings.Contains(a, "claude") || strings.Contains(a, "codex")) || !convoUUID.MatchString(session) {
		delete(p.ended, pane)
		return
	}
	if was, ok := p.ended[pane]; ok && was.session == session {
		was.told = time.Now()
		p.ended[pane] = was
		return
	}
	p.ended[pane] = cmdPane{session: session, from: time.Now(), told: time.Now()}
	p.soon = time.Now().Add(p.soonBy)
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// decorate tells each Claude agent done or idle its commands, while what
// was found is recent.
func (p *cmdProber) decorate(sum *herdrSummary) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at.IsZero() || time.Since(p.at) > p.kept {
		return
	}
	for i := range sum.Workspaces {
		for j := range sum.Workspaces[i].Agents {
			a := &sum.Workspaces[i].Agents[j]
			if c := p.ofLocked(a.PaneID, a.Session); len(c) > 0 {
				a.BgCmds, a.BgCmdList = len(c), c[:min(len(c), maxCmdsShown)]
			}
		}
	}
}

// of: the commands pane's agent (of session; "" any) left running, while
// what was found is recent; nil where none or not known.
func (p *cmdProber) of(pane, session string) []bgCmd {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at.IsZero() || time.Since(p.at) > p.kept {
		return nil
	}
	return p.ofLocked(pane, session)
}

func (p *cmdProber) ofLocked(pane, session string) []bgCmd {
	e, ok := p.ended[pane]
	if !ok || (session != "" && session != e.session) || p.at.Before(e.from) {
		return nil // (a look begun before its turn was over: what it ran in front then, maybe)
	}
	return p.cmds[e.session]
}

// seen: what the last look found pane's agent (of session) left running,
// where that look tells it for sure: whole (none cut short, on a host
// whose processes can be read: see parseCmdScan), recent, and begun after
// the pane came to done or idle — after anything its turns set going.
func (p *cmdProber) seen(pane, session string) ([]bgCmd, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.ended[pane]
	if !ok || session == "" || e.session != session || p.at.IsZero() || time.Since(p.at) > p.kept || p.at.Before(e.from) {
		return nil, false
	}
	return p.cmds[e.session], true
}

// run looks when due, until ctx ends.
func (p *cmdProber) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var last time.Time
	for {
		p.mu.Lock()
		wait := time.Hour
		if len(p.ended) > 0 {
			next := last.Add(p.every)
			if !p.soon.IsZero() && p.soon.Before(next) {
				next = p.soon
			}
			wait = max(0, time.Until(next))
		}
		p.mu.Unlock()
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			continue
		case <-timer.C:
		}
		began := time.Now()
		p.mu.Lock()
		if p.forget > 0 {
			for pane, e := range p.ended {
				if began.Sub(e.told) > p.forget {
					delete(p.ended, pane)
				}
			}
		}
		due := len(p.ended) > 0
		if !p.soon.After(began) {
			p.soon = time.Time{} // (else it is due still: one coming to done or idle since the last)
		}
		p.mu.Unlock()
		if !due {
			continue
		}
		last = began
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		found, whole, err := p.scan(sctx)
		cancel()
		p.mu.Lock()
		changed := false
		if err != nil || !whole {
			// (Not known, or not all of it: nothing told, rather than what was.)
			changed = !p.at.IsZero()
			p.at, p.cmds = time.Time{}, nil
		} else {
			changed = p.at.IsZero() || !sameCmds(p.cmds, found)
			// (Or the same, but now told of a pane it was not before: one
			// come to done or idle after the last look began.)
			for _, e := range p.ended {
				if p.at.Before(e.from) && !began.Before(e.from) && len(found[e.session]) > 0 {
					changed = true
				}
			}
			p.at, p.cmds = began, found // (as of when the look began)
		}
		poke := p.poke
		p.mu.Unlock()
		if changed && poke != nil {
			poke()
		}
	}
}

func sameCmds(a, b map[string][]bgCmd) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// cmdScanCommand prints the commands Claude Code processes of the SSH
// account run in the background: a line "cmd<TAB><pid><TAB><start
// ticks><TAB><where its output goes, or "monitor:<session>"><TAB><its
// arguments from eval on>"
// each, "clock<TAB><boot time><TAB><ticks a second>", and "more" where it
// stopped short of the rest. The Claude Code
// processes: named claude, or node running Claude Code's program; their
// children only are looked at (as the kernel lists each thread's, else by
// pgrep -P one at a time: a pgrep of BusyBox takes no list, nor, in older
// ones, -u: whose each is, is looked at here). Not this script itself,
// which names Claude Code's program too (its children, its subshells, run
// as Claude Code's shell would: "&& eval '" … "pwd -P >| "). Fixed script.
func cmdScanCommand() string {
	script := `btime=$(sed -n 's/^btime \([0-9]*\)$/\1/p' /proc/stat 2>/dev/null); hz=$(getconf CLK_TCK 2>/dev/null)
printf 'clock\t%s\t%s\n' "$btime" "$hz"
if command -v pgrep >/dev/null 2>&1; then
  claudes=$( { pgrep -x claude; pgrep -f '[c]laude-code/cli'; pgrep -x codex; } 2>/dev/null | sort -u | grep -vx "$$")
  kids=$(for c in $claudes; do
    [ -O "/proc/$c" ] || continue
    # (Read twice: a child ending while the list is read may leave others out of that reading.)
    { cat /proc/"$c"/task/*/children && cat /proc/"$c"/task/*/children; } 2>/dev/null || pgrep -P "$c" 2>/dev/null
  done | tr ' ' '\n' | sort -u)
else
  claudes=" $(for d in /proc/[0-9]*; do
    [ -O "$d" ] || continue
    case "$(head -c 16 "$d/comm" 2>/dev/null) $(tr '\0' ' ' <"$d/cmdline" 2>/dev/null)" in claude\ *|codex\ *|*claude-code"/cli"*) printf '%s ' "${d#/proc/}";; esac
  done)"
  kids=$(for d in /proc/[0-9]*; do case $claudes in *" $(sed 's/^.*) //' "$d/stat" 2>/dev/null | cut -d' ' -f2) "*) echo "${d#/proc/}";; esac; done)
fi
n=0
for k in $kids; do
  l=$(readlink "/proc/$k/fd/1" 2>/dev/null) || continue
  case $l in
  */tasks/*.output|*/tasks/*.output" (deleted)") ;;
  *)
    case $(tr '\0\n' '  ' <"/proc/$k/cmdline" 2>/dev/null) in
    *"&& eval '"*"pwd -P >| "*)
      s=$(tr '\n\0' ' \n' <"/proc/$k/environ" 2>/dev/null | sed -n 's/^CLAUDE_CODE_SESSION_ID=\([0-9a-fA-F-]*\)$/\1/p' | head -n 1)
      [ -n "$s" ] || continue
      l="monitor:$s";;
    *)
      # (A command a Codex runs — its app-server's or its own child, of a
      # tool call of a thread: by that thread.)
      e=$(tr '\n\0' ' \n' <"/proc/$k/environ" 2>/dev/null)
      printf '%s\n' "$e" | grep -q '^CODEX_TOOL_CALL_ID=.' || continue
      s=$(printf '%s\n' "$e" | sed -n 's/^CODEX_THREAD_ID=\([0-9a-fA-F-]*\)$/\1/p' | head -n 1)
      [ -n "$s" ] || continue
      l="codex:$s";;
    esac;;
  esac
  [ "$n" -lt 256 ] || { echo more; exit 0; }
  n=$((n + 1))
  st=$(sed 's/^.*) //' "/proc/$k/stat" 2>/dev/null | cut -d' ' -f20)
  case $l in
  codex:*) t=$(tr '\0\n\t' '   ' <"/proc/$k/cmdline" 2>/dev/null | sed 's/^[^ ]*sh -l\{0,1\}c //' | head -c 400);; # (a shell's command; else the program itself, run as it is)
  *) t=$(tr '\0\n\t' '   ' <"/proc/$k/cmdline" 2>/dev/null | awk -v "m=&& eval '" '` + cmdEvalAwk + `' | head -c 400);;
  esac
  printf 'cmd\t%s\t%s\t%s\t%s\n' "$k" "$st" "$(printf '%s' "$l" | tr '\t\n' '  ')" "$t"
done
exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral"
}

// cmdEvalAwk: what follows the first "&& eval '" of a line (Claude Code's
// shell runs the command so, after setting itself up; the command may hold
// another).
const cmdEvalAwk = `{ i = index($0, m); if (i) print substr($0, i + length(m)) }`

// cmdCodex: what a Codex's command's line says in place of where its output
// goes: the thread it was run for.
var cmdCodex = regexp.MustCompile(`^codex:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// codexSandboxWrapper: a Codex's process that holds a command in its
// sandbox (Linux: codex-linux-sandbox, and bwrap under it; macOS:
// sandbox-exec), the command their child, found as well — 0.167.2,
// sandbox workspace-write, 11-15.
func codexSandboxWrapper(cmdline string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(cmdline), " ")
	base := first[strings.LastIndex(first, "/")+1:]
	return base == "codex-linux-sandbox" || base == "bwrap" || base == "sandbox-exec"
}

// cmdMonitor: what a Monitor's line says in place of where its output goes.
var cmdMonitor = regexp.MustCompile(`^monitor:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

var cmdPID = regexp.MustCompile(`^[0-9]{1,10}$`)

var cmdOutput = regexp.MustCompile(`/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})/tasks/([A-Za-z0-9_-]{1,64})\.output( \(deleted\))?$`)

// parseCmdScan reads what cmdScanCommand printed: each session's commands,
// one per task (the earliest begun of its processes), in the order begun;
// whole false where the scan was cut short (not all of them are there), or
// the host's processes could not be read at all (no /proc: no boot time).
func parseCmdScan(out string) (cmds map[string][]bgCmd, whole bool) {
	whole = true
	var btime, hz int64
	type found struct {
		session string
		cmd     bgCmd
		start   int64
	}
	var all []found
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 5)
		switch {
		case line == "more":
			whole = false
		case len(f) == 3 && f[0] == "clock":
			btime, _ = strconv.ParseInt(f[1], 10, 64)
			hz, _ = strconv.ParseInt(f[2], 10, 64)
		case len(f) == 5 && f[0] == "cmd":
			start, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil || start < 0 {
				continue
			}
			var session, task, out string
			if m := cmdOutput.FindStringSubmatch(f[3]); m != nil {
				session, task = m[1], m[2]
				if m[3] == "" && strings.HasPrefix(f[3], "/") {
					out = f[3] // (deleted: nothing there to read)
				}
			} else if m := cmdMonitor.FindStringSubmatch(f[3]); m != nil && cmdPID.MatchString(f[1]) {
				session, task = m[1], "monitor-"+f[1] // (its task's name is not told: its process's, for one)
			} else if m := cmdCodex.FindStringSubmatch(f[3]); m != nil && cmdPID.MatchString(f[1]) {
				if codexSandboxWrapper(f[4]) {
					continue // (its sandbox's process around the command, found as well: one command, not two)
				}
				session, task = m[1], "codex-"+f[1]
			} else {
				continue
			}
			all = append(all, found{session: strings.ToLower(session), cmd: bgCmd{Task: task, Title: cmdTitle(f[4]), Out: out}, start: start})
		}
	}
	if btime <= 0 {
		whole = false // (no /proc: no process of any agent found, which is not none running)
	}
	slices.SortStableFunc(all, func(a, b found) int { return int(min(max(a.start-b.start, -1), 1)) })
	got := map[string][]bgCmd{}
	for _, c := range all {
		if slices.ContainsFunc(got[c.session], func(o bgCmd) bool { return o.Task == c.cmd.Task }) {
			continue // (a process of a task already told: its shell's child)
		}
		if btime > 0 && hz > 0 && hz <= 100000 && c.start < (1<<62)/1000 {
			c.cmd.Since = btime*1000 + c.start*1000/hz
		}
		got[c.session] = append(got[c.session], c.cmd)
	}
	return got, whole
}

// cmdTitle: what a command runs, from its shell's arguments from eval on
// ("'<command>' < /dev/null && pwd -P >| …" as Claude Code words it; of
// a long one, its start), on one line; "" when it cannot be told.
func cmdTitle(s string) string {
	if i := strings.LastIndex(s, "' < /dev/null"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, `'"'"'`, "'")
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
	return truncate(s, 80)
}

// cmdSession: what pane's agent's commands left running are found by — a
// Claude Code's session (sid); a Codex's thread, only where the short look
// tells it for sure (codexPaneLooks.threadOf): a thread told wrong would
// show another conversation's commands as this one's.
func (s *Server) cmdSession(sess *term.Session, pane, agent, title, sid string) string {
	if !isCodexAgent(agent) {
		return sid
	}
	thread, stale := s.codexPanes.threadOf(sess.ID, pane, title)
	if stale {
		// (Looked again, not waited for: what it finds is for the next
		// summary. One at a time a session; the look's own cache shares it.)
		if socket := s.herdrSocks.get(sess.ID); socket != "" && !threadLookTaken(&s.codexThreadLooks, sess.ID) {
			go func() {
				defer s.codexThreadLooks.Delete(sess.ID)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_, _ = s.codexPanes.since(ctx, sess, socket, time.Now().Add(-codexThreadFresh))
			}()
		}
	}
	return thread
}

// threadLookTaken: a look for cmdSession is under way for this session
// (else it is now taken: the caller begins it, and lets it go when done).
func threadLookTaken(m *sync.Map, sessID string) bool {
	_, taken := m.LoadOrStore(sessID, true)
	return taken
}
