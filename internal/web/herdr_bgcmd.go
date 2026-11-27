package web

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
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
	maxCmdsShown           = 8                   // listed per agent (all are counted; the card lists each, 11-15)
	cmdScanMax             = 512 << 10           // (with the subagents out and the background ids of those at work: review of 11-25)
	cmdScanTotalBudget     = cmdScanMax - 16<<10 // unified script output budget below cmdScanMax so overflow is prevented
	cliScanBudget          = 64 << 10            // total byte budget for cli\t lines per scan
	cliScanMaxLines        = 64                  // max cli\t lines emitted per scan
	maxBgCmdMatchBytes     = 4 << 10             // max full command length kept for synthetic task exact matching
	subCandPerSession      = 128                 // max candidate subagent files streamed per watched session
	subCandBytesPerSession = 32 << 10            // max cumulative candidate path bytes streamed per watched session
	subCandTotalMax        = 1024                // max candidate subagent files streamed across all watched sessions
	subCandTotalBytes      = 256 << 10           // max cumulative candidate path bytes across all watched sessions
	cliLogKeep             = 30 * time.Minute    // keep verified CLI log paths briefly so an open panel stays readable after exit
	cliLogsMax             = 512
)

// errScanBudget marks a scan cut short specifically by the script's unified
// output byte budget ("budget" marker), as distinct from SSH/host failures or
// >256-process truncation ("more").
var errScanBudget = errors.New("background scan hit output budget")

// bgCLI is a known agent CLI process ("agy", "claude", "codex") discovered
// inside a background command's process tree, with its verified session UUID
// and host log path (kept server-side, never sent to the browser).
type bgCLI struct {
	Agent   string
	Session string
	Log     string
}

type bgCLILogEntry struct {
	log    string
	seenAt time.Time
}

// bgCmd is a command running in the background: its task, what it runs
// (as far as it could be told), and when it began (unix ms).
type bgCmd struct {
	Task  string `json:"task"`
	Title string `json:"title,omitempty"`
	Since int64  `json:"since,omitempty"`
	// Out: the file its output goes to (none for a Monitor's), for the page
	// to show its end, as of one the log told of.
	Out string `json:"out,omitempty"`
	// Desc: what the agent said the command does (a Claude Code call's
	// description), where its log tells it; the page names it so.
	Desc string `json:"desc,omitempty"`
	// quiet: a Monitor's — in the background by its nature, whatever the
	// agent's log says (see bgTaskIDs). (Not sent.)
	quiet bool
	// clis: known agent CLIs with resolvable sessions found in this command's
	// process tree. Non-empty promotes the command to background subagent(s).
	clis []bgCLI
	// matchCmd: cleaned full command (up to maxBgCmdMatchBytes) kept separately
	// from the 80-char display Title for exact synthetic task matching.
	// matchCut is true when the raw command exceeded maxBgCmdMatchBytes so it
	// must not participate in synthetic upgrade matching.
	matchCmd string
	matchCut bool
}

type cmdProber struct {
	mu    sync.Mutex
	ended map[string]cmdPane // the Claude panes done or idle
	// read: the conversations a chat page reads, at rest by their own logs
	// (Codex's: what they left running checked against the host), by
	// their session; looked for while read (see readAt), not after.
	read map[string]cmdPane
	// working: the Claude panes at work — what they set going in the
	// background meanwhile (a dev server begun, a subagent sent off) is told
	// too, for the side list (UI item 8, 11-25).
	working map[string]cmdPane
	cmds    map[string][]bgCmd   // agent session → its commands, as last found
	subs    map[string][]bgAgent // agent session → its subagents at work in the background, as last found (see parseBgSubScan)
	// cliLogs: server-side verified log paths of CLI subagents seen in recent
	// scans, keyed by "<agent>\x00<session>".
	cliLogs map[string]bgCLILogEntry
	at      time.Time // when they were found (zero: not known)
	soon    time.Time // a look due soon (a pane came to done or idle)
	wake    chan struct{}
	poke    func()
	scan func(ctx context.Context, watched []string) (cmds map[string][]bgCmd, subs map[string][]bgAgent, bgIDs map[string]map[string]bool, whole bool, err error)
	// bgIDs: of the sessions at work, which of their commands running the
	// last look found their log says they set going in the background.
	bgIDs map[string]map[string]bool

	every, soonBy, kept time.Duration // (cmdEvery, cmdSoon, cmdKept)
	// forget: a pane not considered again for this long is no longer looked
	// for (0: kept till it is said to work again). For panes whose status is
	// told only while someone reads them (a tmux session's chat): none read,
	// no looks.
	forget time.Duration
}

type cmdPane struct {
	session string
	mark    string    // (a read conversation's: what it left running, as read)
	from    time.Time // when it came to done or idle: what a look begun before found is not told of it
	told    time.Time // when it was last considered (see forget)
}

func newCmdProber(scan func(context.Context) (map[string][]bgCmd, bool, error)) *cmdProber {
	var both func(context.Context, []string) (map[string][]bgCmd, map[string][]bgAgent, map[string]map[string]bool, bool, error)
	if scan != nil {
		both = func(ctx context.Context, _ []string) (map[string][]bgCmd, map[string][]bgAgent, map[string]map[string]bool, bool, error) {
			c, whole, err := scan(ctx)
			return c, nil, nil, whole, err
		}
	}
	return newCmdSubProber(both)
}

// newCmdSubProber: one whose look tells the subagents at work as well, and,
// of the sessions at work (working: those it is given), which commands are
// in the background.
func newCmdSubProber(scan func(ctx context.Context, watched []string) (map[string][]bgCmd, map[string][]bgAgent, map[string]map[string]bool, bool, error)) *cmdProber {
	return &cmdProber{ended: map[string]cmdPane{}, working: map[string]cmdPane{}, bgIDs: map[string]map[string]bool{}, wake: make(chan struct{}, 1), scan: scan, every: cmdEvery, soonBy: cmdSoon, kept: cmdKept}
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
	if status == herdr.StatusWorking && strings.Contains(strings.ToLower(agent), "claude") && convoUUID.MatchString(session) {
		delete(p.ended, pane)
		session = strings.ToLower(session)
		if was, ok := p.working[pane]; ok && was.session == session {
			was.told = time.Now()
			p.working[pane] = was
			return
		}
		p.working[pane] = cmdPane{session: session, from: time.Now(), told: time.Now()}
		p.soon = time.Now().Add(p.soonBy)
		select {
		case p.wake <- struct{}{}:
		default:
		}
		return
	}
	delete(p.working, pane)
	if a := strings.ToLower(agent); !ended || !(strings.Contains(a, "claude") || strings.Contains(a, "codex") || strings.Contains(a, "agy") || strings.Contains(a, "antigravity")) || !convoUUID.MatchString(session) {
		delete(p.ended, pane)
		return
	}
	session = strings.ToLower(session)
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

// nudge asks the prober to scan soon (within soonBy) when a conversation read
// notices background work starting or ending, instead of waiting up to every
// (1 minute) while the agent stays working.
func (p *cmdProber) nudge() {
	p.mu.Lock()
	if len(p.working) == 0 && len(p.ended) == 0 && len(p.read) == 0 {
		p.mu.Unlock()
		return
	}
	soon := time.Now().Add(p.soonBy)
	if p.soon.IsZero() || soon.Before(p.soon) {
		p.soon = soon
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// readAt: a chat page read session's conversation (a Codex thread), at rest
// or not as its log says (idle), leaving running what mark says (its
// commands' ids): what it left running is looked for while it is, as of
// when it came to rest so — another mark (a turn read whole between two
// reads, that set more going: review r2a-2) is a rest anew.
func (p *cmdProber) readAt(session string, idle bool, mark string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !idle || !convoUUID.MatchString(session) {
		delete(p.read, session)
		return
	}
	if p.read == nil {
		p.read = map[string]cmdPane{}
	}
	if was, ok := p.read[session]; ok && was.mark == mark {
		was.told = time.Now()
		p.read[session] = was
		return
	}
	p.read[session] = cmdPane{session: session, mark: mark, from: time.Now(), told: time.Now()}
	p.soon = time.Now().Add(p.soonBy)
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// readSeen: what the last look found session left running, where that
// look tells it for sure: whole (none cut short, on a host whose processes
// can be read), recent, begun after its conversation came to rest as read.
func (p *cmdProber) readSeen(session string) ([]bgCmd, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.read[session]
	if !ok || p.at.IsZero() || time.Since(p.at) > p.kept || p.at.Before(e.from) {
		return nil, false
	}
	return p.cmds[session], true
}

// readForget: a conversation no page has read for this long is no longer
// looked for.
const readForget = cmdKept

// decorate tells each agent done, idle, or at work its background commands and
// background subagents (including recognized agent CLIs running inside
// background commands), while what was found is recent.
func (p *cmdProber) decorate(sum *herdrSummary) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at.IsZero() || time.Since(p.at) > p.kept {
		return
	}
	for i := range sum.Workspaces {
		for j := range sum.Workspaces[i].Agents {
			a := &sum.Workspaces[i].Agents[j]
			activeCmds := p.activeCmdsLocked(a.PaneID, a.Session)
			plain, cliSubs := splitBgCmds(activeCmds)
			if len(plain) > 0 {
				a.BgCmds, a.BgCmdList = len(plain), plain[:min(len(plain), maxCmdsShown)]
				if len(plain) > maxCmdsShown {
					a.BgCmdTasks = bgCmdTaskIDs(plain)
				}
			}
			var nativeSubs []bgAgent
			if sess := p.paneSessionLocked(a.PaneID, a.Session); sess != "" {
				nativeSubs = subsShown(p.subs[sess], a.Status, a.Bg, a.BgN)
			}
			if len(nativeSubs) > 0 || len(cliSubs) > 0 {
				if len(cliSubs) > maxCmdsShown {
					cliSubs = cliSubs[:maxCmdsShown]
				}
				merged := make([]bgAgent, 0, len(nativeSubs)+len(cliSubs))
				merged = append(merged, nativeSubs...)
				merged = append(merged, cliSubs...)
				a.BgAgentList = merged
			}
		}
	}
}

func (p *cmdProber) activeCmdsLocked(pane, session string) []bgCmd {
	if c := p.ofLocked(pane, session); len(c) > 0 {
		return c
	}
	// (At work: those it set going in the background only — not what it runs in front now.)
	if e, ok := p.working[pane]; ok && (session == "" || strings.ToLower(session) == e.session) && !p.at.Before(e.from) {
		var quiet []bgCmd
		for _, c := range p.cmds[e.session] {
			if c.quiet || p.bgIDs[e.session][c.Task] {
				quiet = append(quiet, c)
			}
		}
		return quiet
	}
	return nil
}

// cliAgentsOf returns recognized background CLI subagents currently active for
// pane/session (used by apiConversation to upgrade running commands).
func (p *cmdProber) cliAgentsOf(pane, session string) []bgAgent {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at.IsZero() || time.Since(p.at) > p.kept {
		return nil
	}
	_, cliSubs := splitBgCmds(p.activeCmdsLocked(pane, session))
	return cliSubs
}

// cliLog returns the host-verified log path for a background CLI subagent
// discovered in a recent scan on this session's host.
func (p *cmdProber) cliLog(agent, sid string) (string, bool) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	sid = strings.ToLower(strings.TrimSpace(sid))
	if agent == "" || !convoUUID.MatchString(sid) {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := agent + "\x00" + sid
	if e, ok := p.cliLogs[key]; ok && time.Since(e.seenAt) <= cliLogKeep && bgCLILogValid(agent, e.log, sid) {
		return e.log, true
	}
	// Fallback to live p.cmds (also convenient for unit tests seeding p.cmds).
	for _, list := range p.cmds {
		for _, c := range list {
			for _, cli := range c.clis {
				if cli.Agent == agent && cli.Session == sid && bgCLILogValid(agent, cli.Log, sid) {
					return cli.Log, true
				}
			}
		}
	}
	return "", false
}

func (p *cmdProber) recordCLILogsLocked(found map[string][]bgCmd, now time.Time) {
	if p.cliLogs == nil {
		p.cliLogs = map[string]bgCLILogEntry{}
	}
	for k, e := range p.cliLogs {
		if now.Sub(e.seenAt) > cliLogKeep {
			delete(p.cliLogs, k)
		}
	}
	for _, list := range found {
		for _, c := range list {
			for _, cli := range c.clis {
				if !bgCLILogValid(cli.Agent, cli.Log, cli.Session) {
					continue
				}
				p.cliLogs[cli.Agent+"\x00"+cli.Session] = bgCLILogEntry{log: cli.Log, seenAt: now}
			}
		}
	}
	for len(p.cliLogs) > cliLogsMax {
		var oldestKey string
		var oldestAt time.Time
		for k, e := range p.cliLogs {
			if oldestKey == "" || e.seenAt.Before(oldestAt) {
				oldestKey, oldestAt = k, e.seenAt
			}
		}
		delete(p.cliLogs, oldestKey)
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

// paneSessionLocked: the agent session pane's Claude Code is watched for (at
// work, or done or idle); "" else. Unlike ofLocked (commands), subagents are
// already filtered to background-only by their meta and gated by subsShown, so
// transitioning between working and done/idle for the same session keeps them
// visible without a 5s gap.
func (p *cmdProber) paneSessionLocked(pane, session string) string {
	e, ok := p.working[pane]
	if !ok {
		e, ok = p.ended[pane]
	}
	if !ok || (session != "" && strings.ToLower(session) != e.session) {
		return ""
	}
	return e.session
}

func (p *cmdProber) ofLocked(pane, session string) []bgCmd {
	e, ok := p.ended[pane]
	if !ok || (session != "" && strings.ToLower(session) != e.session) || p.at.Before(e.from) {
		return nil // (a look begun before its turn was over: what it ran in front then, maybe)
	}
	return p.cmds[e.session]
}

// run looks when due, until ctx ends.
func (p *cmdProber) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var last time.Time
	for {
		p.mu.Lock()
		wait := time.Hour
		if len(p.ended) > 0 || len(p.read) > 0 || len(p.working) > 0 {
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
		for sid, e := range p.read {
			if began.Sub(e.told) > readForget {
				delete(p.read, sid)
			}
		}
		due := len(p.ended) > 0 || len(p.read) > 0 || len(p.working) > 0
		// (The sessions it is for: at work, "w:", their background commands told apart; at rest, "s:".)
		var watched []string
		for _, e := range p.working {
			if !slices.Contains(watched, "w:"+e.session) {
				watched = append(watched, "w:"+e.session)
			}
		}
		for _, e := range p.ended {
			if !slices.Contains(watched, "s:"+e.session) && !slices.Contains(watched, "w:"+e.session) {
				watched = append(watched, "s:"+e.session)
			}
		}
		if !p.soon.After(began) {
			p.soon = time.Time{} // (else it is due still: one coming to done or idle since the last)
		}
		p.mu.Unlock()
		if !due {
			continue
		}
		last = began
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		found, subs, ids, whole, err := p.scan(sctx, watched)
		cancel()
		p.mu.Lock()
		// (What this look found, in place of the last's: one ended, or of a session no longer at
		// work, goes with it — review of 11-25.)
		// On output-budget overflow (ErrOutputTooLarge or script budget marker),
		// keep the last known lists intact instead of wiping the whole host's
		// background tasks; they still age out naturally after p.kept if
		// overflow persists.
		var changed bool
		if errors.Is(err, sshx.ErrOutputTooLarge) || errors.Is(err, errScanBudget) {
			changed = false
		} else {
			if ids == nil {
				ids = map[string]map[string]bool{}
			}
			changed = !sameIDs(p.bgIDs, ids)
			p.bgIDs = ids
			if err != nil || !whole {
				// (Not known, or not all of it: nothing told, rather than what was.)
				changed = changed || !p.at.IsZero()
				p.at, p.cmds, p.subs = time.Time{}, nil, nil
			} else {
				changed = changed || p.at.IsZero() || !sameCmds(p.cmds, found) || !sameSubs(p.subs, subs)
				// (Or the same, but now told of a pane it was not before: one
				// come to done or idle after the last look began.)
				for _, e := range p.ended {
					if p.at.Before(e.from) && !began.Before(e.from) && (len(found[e.session]) > 0 || len(subs[e.session]) > 0) {
						changed = true
					}
				}
				for _, e := range p.working {
					if p.at.Before(e.from) && !began.Before(e.from) && (len(found[e.session]) > 0 || len(subs[e.session]) > 0) {
						changed = true
					}
				}
				p.at, p.cmds, p.subs = began, found, subs // (as of when the look began)
				p.recordCLILogsLocked(found, began)
			}
		}
		poke := p.poke
		p.mu.Unlock()
		if changed && poke != nil {
			poke()
		}
	}
}

func sameIDs(a, b map[string]map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if len(v) != len(b[k]) {
			return false
		}
		for id := range v {
			if !b[k][id] {
				return false
			}
		}
	}
	return true
}

func sameBgCmd(a, b bgCmd) bool {
	return a.Task == b.Task && a.Title == b.Title && a.Since == b.Since && a.Out == b.Out && a.Desc == b.Desc && a.quiet == b.quiet && a.matchCmd == b.matchCmd && a.matchCut == b.matchCut && slices.Equal(a.clis, b.clis)
}

func sameCmds(a, b map[string][]bgCmd) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.EqualFunc(v, b[k], sameBgCmd) {
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
const agyBgFdsSh = `
agy_bg_fds() {
  agy_fds "$1"
  [ -n "$agylog" ] && return 0
  agysid= agylog= agybase=
  _ab_nl=$(printf '\nx'); _ab_nl=${_ab_nl%x}
  for _ab_fd in "$1"/fd/*; do
    _ab_l=$(readlink "$_ab_fd" 2>/dev/null) || continue
    case $_ab_l in *"$_ab_nl"*) continue;; esac
    _ab_p=$_ab_l
    case $_ab_p in
    */conversations/????????-????-????-????-????????????.db) _ab_p=${_ab_p%.db};;
    */conversations/????????-????-????-????-????????????.db-wal) _ab_p=${_ab_p%.db-wal};;
    */conversations/????????-????-????-????-????????????.db-shm) _ab_p=${_ab_p%.db-shm};;
    *) continue;;
    esac
    _ab_id=${_ab_p##*/}
    case $_ab_id in *[!0-9a-fA-F-]*) continue;; esac
    _ab_id=$(printf '%s' "$_ab_id" | tr 'A-F' 'a-f')
    _ab_base=${_ab_l%/conversations/*}
    _ab_cand="$_ab_base/brain/$_ab_id/.system_generated/logs/transcript_full.jsonl"
    if [ -f "$_ab_cand" ]; then
      if [ -z "$agylog" ] || [ "$_ab_cand" -nt "$agylog" ]; then
        agysid=$_ab_id
        agylog=$_ab_cand
      fi
    fi
  done
}
`

func cmdScanCommand(watched ...string) string {
	return cmdScanCommandWithBudgets(cmdScanTotalBudget, cliScanBudget, watched...)
}

func cmdScanCommandWithBudget(budget int, watched ...string) string {
	return cmdScanCommandWithBudgets(cmdScanTotalBudget, budget, watched...)
}

// cmdScanCommandWithBudgets builds the host scan script with a unified total
// byte budget (totalBudget <= cmdScanMax) and priority emission order:
//   1. clock + plain cmd / anc / bgid / desc lines (highest priority)
//   2. sub lines (truncating/omitting subagent description first if tight)
//   3. cli lines (lowest priority, capped at min(cliBudget, remaining) and cliScanMaxLines)
func cmdScanCommandWithBudgets(totalBudget, cliBudget int, watched ...string) string {
	if totalBudget < 0 {
		totalBudget = 0
	}
	if cliBudget < 0 {
		cliBudget = 0
	}
	script := claudeConfigDirsSh + `LC_ALL=C
_cldirs_loaded=0
_ensure_cldirs() {
  if [ "$_cldirs_loaded" = 0 ]; then claude_cfg_dirs; _cldirs_loaded=1; fi
}
# _sole_claude_log <sid>: sets _best to the sole matching log across $cldirs
# and _lfound to the match count (0, 1, or 2 for ambiguous). Never guesses by
# mtime, and returns empty when config dir enumeration was incomplete.
_sole_claude_log() {
  _best= _lfound=0
  _ensure_cldirs
  [ "$cldirs_incomplete" = 0 ] || return 0
  while IFS= read -r _cd; do
    [ -n "$_cd" ] || continue
    for _f in "$_cd"/projects/*/"$1".jsonl; do
      [ -f "$_f" ] || continue
      _lfound=$((_lfound + 1))
      if [ "$_lfound" -eq 1 ]; then
        _best=$_f
      else
        _best=
        break
      fi
    done
    [ "$_lfound" -le 1 ] || break
  done <<CORRAL_CLDIRS
$cldirs
CORRAL_CLDIRS
}
_sc_rem=` + strconv.Itoa(totalBudget) + `
_sc_bcut=0
_sc_mark_budget() {
  [ "$_sc_bcut" = 1 ] || { _sc_bcut=1; printf 'budget\n'; }
}
_sc_emit() {
  _sc_l=$((${#1} + 1))
  if [ "$_sc_l" -le "$_sc_rem" ]; then
    printf '%s\n' "$1"
    _sc_rem=$((_sc_rem - _sc_l))
    return 0
  fi
  return 1
}
_sc_find_sub() {
  if find "$1" -maxdepth 0 -printf '' >/dev/null 2>&1; then
    find "$1" -maxdepth 1 -name 'agent-*.jsonl' -mmin -360 -printf '%T@\t%p\n' 2>/dev/null |
      sort -rn | cut -f2-
  else
    # Fallback without GNU find -printf (e.g. BusyBox): cannot sort by mtime, so
    # report incomplete once candidates exceed subsScanned (16) to keep old state.
    find "$1" -maxdepth 1 -name 'agent-*.jsonl' -mmin -360 2>/dev/null |
      awk -v maxn=` + strconv.Itoa(subsScanned) + ` 'NR > maxn { print "UNSORTED_CAP"; exit } { print }'
  fi
}
btime=$(sed -n 's/^btime \([0-9]*\)$/\1/p' /proc/stat 2>/dev/null); hz=$(getconf CLK_TCK 2>/dev/null)
_sc_emit "$(printf 'clock\t%s\t%s' "$btime" "$hz")" || true
` + codexFdsSh + agyFdsSh + agyBgFdsSh + `
codex_bg_fds() {
  cxid= cxlog=
  codex_fds "$1"
  if [ -n "$cxlog" ] && [ -f "$cxlog" ]; then
    codex_log_sid "$cxlog"
    [ -n "$cxid" ] && return 0
    cxlog=
  fi
  if [ -n "$cxpend" ]; then
    for _xb_f in "$cxhome"/sessions/*/*/*/rollout-*-"$cxpend".jsonl; do
      [ -f "$_xb_f" ] || continue
      codex_log_sid "$_xb_f"
      if [ -n "$cxid" ]; then cxlog=$_xb_f; return 0; fi
    done
  fi
  cxid= cxlog=
}
claude_bg_fds() {
  ccsid= cclog=
  _cb_p=$1
  _cb_env=$(tr '\0\n' '\n\001' <"/proc/$_cb_p/environ" 2>/dev/null)
  if printf '%s\n' "$_cb_env" | grep -qx 'CORRAL_SIDE_COPY=1'; then return 0; fi
  _cb_cfg=$(printf '%s\n' "$_cb_env" | sed -n 's/^CLAUDE_CONFIG_DIR=//p' | head -n 1)
  [ -n "$_cb_cfg" ] || _cb_cfg=$HOME/.claude
  _cb_cfg=${_cb_cfg%/}
  _cb_sf="$_cb_cfg/sessions/$_cb_p.json"
  if [ -f "$_cb_sf" ]; then
    ccsid=$(head -c 65536 "$_cb_sf" 2>/dev/null | tr -d '\n' | grep -oE '"sessionId"[ ]*:[ ]*"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"' | head -n 1 | cut -d'"' -f4 | tr 'A-F' 'a-f')
  fi
  if [ -z "$ccsid" ]; then
    _cb_nl=$(printf '\nx'); _cb_nl=${_cb_nl%x}
    for _cb_fd in "/proc/$_cb_p"/fd/*; do
      _cb_l=$(readlink "$_cb_fd" 2>/dev/null) || continue
      case $_cb_l in
      *"$_cb_nl"*|*/subagents/*) continue;;
      "$_cb_cfg"/projects/*/????????-????-????-????-????????????.jsonl)
        _cb_id=${_cb_l##*/}; _cb_id=${_cb_id%.jsonl}
        case $_cb_id in *[!0-9a-fA-F-]*) continue;; esac
        ccsid=$(printf '%s' "$_cb_id" | tr 'A-F' 'a-f')
        cclog=$_cb_l
        break;;
      esac
    done
  fi
  if [ -z "$ccsid" ]; then
    ccsid=$(tr '\0' '\001' <"/proc/$_cb_p/cmdline" 2>/dev/null | awk 'BEGIN { RS = "\001" }
      NR == 1 { next }
      /^--no-session-persistence$/ { nopersist = 1; next }
      want { val = $0; want = 0; next }
      /^(--session-id|--resume|-r)$/ { want = 1; next }
      /^--session-id=/ { val = substr($0, 14); next }
      /^--resume=/ { val = substr($0, 10); next }
      END { if (!nopersist && val ~ /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/) print tolower(val) }')
  fi
  if [ -n "$ccsid" ] && [ -z "$cclog" ]; then
    for _cb_f in "$_cb_cfg"/projects/*/"$ccsid".jsonl; do
      [ -f "$_cb_f" ] || continue
      cclog=$_cb_f
      break
    done
  fi
  if [ -z "$cclog" ] || [ ! -f "$cclog" ]; then ccsid= cclog=; fi
}
_bt_budget=` + strconv.Itoa(cliBudget) + `
_bt_emitted=0
_bt_tab=$(printf '\t')
_bt_nl=$(printf '\nx'); _bt_nl=${_bt_nl%x}
scan_cmd_tree() {
  [ "$_sc_bcut" = 0 ] || return 0
  _bt_root=$1
  _bt_q=" $_bt_root "
  _bt_vis=" "
  _bt_cnt=0
  _bt_depth=0
  while [ -n "$_bt_q" ] && [ "$_bt_cnt" -lt 64 ] && [ "$_bt_depth" -lt 16 ] && [ "$_sc_bcut" = 0 ]; do
    _bt_depth=$((_bt_depth + 1))
    _bt_next=
    for _bt_p in $_bt_q; do
      [ "$_sc_bcut" = 0 ] || break
      case $_bt_vis in *" $_bt_p "*) continue;; esac
      if [ "$_bt_cnt" -ge 64 ]; then
        _sc_mark_budget
        break
      fi
      _bt_vis="$_bt_vis$_bt_p "
      _bt_cnt=$((_bt_cnt + 1))
      [ -O "/proc/$_bt_p" ] || continue
      _bt_comm=$(head -c 64 "/proc/$_bt_p/comm" 2>/dev/null | tr -d '\r\n\t')
      _bt_exe=$(readlink "/proc/$_bt_p/exe" 2>/dev/null | tr -d '\r\n\t')
      _bt_eclean=${_bt_exe% (deleted)}
      _bt_eb=${_bt_eclean##*/}
      _bt_argv=$(tr '\0\n\t' '\001  ' <"/proc/$_bt_p/cmdline" 2>/dev/null | head -c 512)
      _bt_kind=
      case $_bt_eb in
      agy) _bt_kind=agy;;
      codex) _bt_kind=codex;;
      claude) _bt_kind=claude;;
      node|nodejs|bun|deno)
        case $_bt_argv in
        *claude*) _bt_kind=claude;;
        *codex*) _bt_kind=codex;;
        *antigravity*|*agy*) _bt_kind=agy;;
        esac;;
      *)
        case $_bt_eclean in
        */claude/versions/*) _bt_kind=claude;;
        *)
          case $_bt_comm in
          agy) _bt_kind=agy;;
          codex) _bt_kind=codex;;
          claude) _bt_kind=claude;;
          esac;;
        esac;;
      esac
      _bt_found=0
      if [ -n "$_bt_kind" ]; then
        _bt_sid= _bt_log=
        case $_bt_kind in
        agy) agy_bg_fds "/proc/$_bt_p"; _bt_sid=$agysid _bt_log=$agylog;;
        codex) codex_bg_fds "/proc/$_bt_p"; _bt_sid=$cxid _bt_log=$cxlog;;
        claude) claude_bg_fds "$_bt_p"; _bt_sid=$ccsid _bt_log=$cclog;;
        esac
        # Never rewrite tabs/newlines in log paths (would alias onto a different file); drop them instead.
        case $_bt_log in *"$_bt_tab"*|*"$_bt_nl"*) _bt_sid= _bt_log=;; esac
        if [ -n "$_bt_sid" ] && [ -n "$_bt_log" ]; then
          _bt_line=$(printf 'cli\t%s\t%s\t%s\t%s\t%s\t%s\t%s' "$_bt_root" "$_bt_p" "$_bt_comm" "$_bt_eclean" "$_bt_sid" "$_bt_log" "$_bt_argv")
          _bt_len=$((${#_bt_line} + 1))
          if [ "$_bt_emitted" -lt ` + strconv.Itoa(cliScanMaxLines) + ` ] && [ "$_bt_len" -le "$_bt_budget" ] && _sc_emit "$_bt_line"; then
            _bt_budget=$((_bt_budget - _bt_len))
            _bt_emitted=$((_bt_emitted + 1))
          else
            _bt_budget=0
            _sc_mark_budget
          fi
          _bt_found=1
        fi
      fi
      [ "$_bt_found" = 1 ] && continue
      _bt_ch=$(cat /proc/"$_bt_p"/task/*/children 2>/dev/null || pgrep -P "$_bt_p" 2>/dev/null)
      for _bt_c in $_bt_ch; do
        case $_bt_c in ''|*[!0-9]*) continue;; esac
        case $_bt_vis in *" $_bt_c "*) continue;; esac
        _bt_next="$_bt_next $_bt_c"
      done
    done
    _bt_q=$_bt_next
  done
  if [ "$_sc_bcut" = 0 ] && [ -n "$_bt_q" ]; then
    _sc_mark_budget
  fi
}
# Priority 1: plain background commands (cmd / anc / bgid / desc).
seen=
pairs=
_sc_cmd_pids=
if command -v pgrep >/dev/null 2>&1; then
  claudes=$( { pgrep -x claude; pgrep -f '[c]laude-code/cli'; pgrep -x codex; pgrep -x agy; } 2>/dev/null | sort -u | grep -vx "$$")
  kids=$(for c in $claudes; do
    [ -O "/proc/$c" ] || continue
    # (Read twice: a child ending while the list is read may leave others out of that reading.)
    { cat /proc/"$c"/task/*/children && cat /proc/"$c"/task/*/children; } 2>/dev/null || pgrep -P "$c" 2>/dev/null
  done | tr ' ' '\n' | sort -u)
else
  claudes=" $(for d in /proc/[0-9]*; do
    [ -O "$d" ] || continue
    case "$(head -c 16 "$d/comm" 2>/dev/null) $(tr '\0' ' ' <"$d/cmdline" 2>/dev/null)" in claude\ *|codex\ *|agy\ *|*claude-code"/cli"*) printf '%s ' "${d#/proc/}";; esac
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
      # (A command a Codex or Antigravity CLI runs in the background.)
      e=$(tr '\n\0' ' \n' <"/proc/$k/environ" 2>/dev/null)
      if printf '%s\n' "$e" | grep -q '^CODEX_TOOL_CALL_ID=.'; then
        s=$(printf '%s\n' "$e" | sed -n 's/^CODEX_THREAD_ID=\([0-9a-fA-F-]*\)$/\1/p' | head -n 1)
        [ -n "$s" ] || continue
        l="codex:$s"
      elif printf '%s\n' "$e" | grep -q '^ANTIGRAVITY_AGENT=1$'; then
        s=$(printf '%s\n' "$e" | sed -n 's/^ANTIGRAVITY_CONVERSATION_ID=\([0-9a-fA-F-]*\)$/\1/p' | head -n 1)
        [ -n "$s" ] || continue
        l="agy:$s"
      else
        continue
      fi;;
    esac;;
  esac
  [ "$n" -lt 256 ] || { echo more; break; }
  n=$((n + 1))
  st=$(sed 's/^.*) //' "/proc/$k/stat" 2>/dev/null | cut -d' ' -f20)
  case $l in
  codex:*|agy:*) t=$(tr '\0\n\t' '   ' <"/proc/$k/cmdline" 2>/dev/null | sed 's/^[^ ]*sh -l\{0,1\}c //' | head -c ` + strconv.Itoa(maxBgCmdMatchBytes+1) + `);; # (a shell's command; else the program itself, run as it is; +1 byte detects >4 KiB cut)
  monitor:*) t=$(tr '\0\n\t' '   ' <"/proc/$k/cmdline" 2>/dev/null | awk -v "m=&& eval '" '` + cmdEvalAwk + `' | head -c ` + strconv.Itoa(maxBgCmdMatchBytes+1) + `);;
  *) t=$(tr '\0\n\t' '   ' <"/proc/$k/cmdline" 2>/dev/null | awk -v "m=&& eval '" '` + cmdEvalAwk + `' | head -c 400);;
  esac
  _sc_line=$(printf 'cmd\t%s\t%s\t%s\t%s' "$k" "$st" "$(printf '%s' "$l" | tr '\t\n' '  ')" "$t")
  if ! _sc_emit "$_sc_line"; then
    _sc_mark_budget
    break
  fi
  _sc_cmd_pids="$_sc_cmd_pids $k"
  case $l in */tasks/*.output*) x=${l##*/tasks/}; seen="$seen ${x%%.output*}"; y=${l%/tasks/*}; pairs="$pairs ${y##*/}/${x%%.output*}";; esac
  case $l in
  codex:*)
    # (Its parents, a few up: a command in Codex's sandbox is told by
    # the sandbox's process it runs under.)
    a=$k; anc=
    for _ in 1 2 3 4 5; do
      a=$(sed 's/^.*) //' "/proc/$a/stat" 2>/dev/null | cut -d' ' -f2)
      case $a in ""|0|1|*[!0-9]*) break;; esac
      anc="$anc,$a"
    done
    _sc_emit "$(printf 'anc\t%s\t%s' "$k" "${anc#,}")" || true;;
  esac
done
# (Of a session at work, which of the commands found running its log says it set going in the
# background — the whole log: one begun an hour ago counts; a command run in front writes to a
# task's file too, and is not one. A grep of a log of 200 MB: some 50 ms.)
for a in "$@"; do
  case $a in w:*) ;; *) continue;; esac
  [ -n "$seen" ] || break
  s=${a#w:}
  _sole_claude_log "$s"
  f=$_best
  [ -f "$f" ] || continue
  _sc_ids=$(grep -o '"backgroundTaskId":"[A-Za-z0-9_-]*"' "$f" | sed 's/^"backgroundTaskId":"//; s/"$//' | sort -u)
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    case " $seen " in *" $id "*) _sc_emit "$(printf 'bgid\t%s\t%s' "$s" "$id")" || break;; esac
  done <<CORRAL_BGIDS
$_sc_ids
CORRAL_BGIDS
done
# (Of each command a Claude Code's task file tells, what the agent said it does — its call's
# "description", as the agent's own page shows it: the call is the one whose result names the
# task, read up to the next call's id — one with none does not take the next one's. At most 16,
# a grep or two of its log each.)
i=0; told=
for p in $pairs; do
  [ "$i" -lt 16 ] || break
  case " $told " in *" $p "*) continue;; esac
  told="$told $p"
  s=${p%%/*}; id=${p#*/}
  case $s in *[!0-9a-fA-F-]*|"") continue;; esac
  case $id in *[!A-Za-z0-9_-]*|"") continue;; esac
  _sole_claude_log "$s"
  f=$_best
  [ -f "$f" ] || continue
  i=$((i + 1))
  tu=$(grep -m 1 -F "\"backgroundTaskId\":\"$id\"" "$f" | grep -o '"tool_use_id":"[A-Za-z0-9_-]*"' | head -n 1 | cut -d'"' -f4)
  [ -n "$tu" ] || continue
  w=$(grep -m 1 -F "\"id\":\"$tu\"" "$f" | sed "s/^.*\"id\":\"$tu\"//; s/\"id\":\".*//; s/\"type\":\"tool_use\".*//" | grep -oE '"description":"([^"\\]|\\.)*"' | head -n 1 | head -c 600)
  [ -n "$w" ] && { _sc_emit "$(printf 'desc\t%s\t%s\t{%s}' "$s" "$id" "$w")" || true; }
done
# Priority 2: native background subagents — bounded newest-first streaming
# (never buffer find output in a shell variable; drop description before
# marking budget cut; mark budget cut if any required sub/cli line is dropped
# or any candidate cap is hit), followed in the same consumer shell by
# Priority 3 (CLI lines) so _sc_rem and _sc_bcut carry over.
{
  for a in "$@"; do
    s=${a#?:}
    _sole_claude_log "$s"
    [ "$_lfound" -eq 1 ] || continue
    for d in "${_best%.jsonl}/subagents"; do
      [ -d "$d" ] || continue
      _sc_find_sub "$d" |
        awk -v s="$s" -v maxn=` + strconv.Itoa(subCandPerSession) + ` -v maxb=` + strconv.Itoa(subCandBytesPerSession) + ` '{
          b += length($0) + 1
          if ($0 == "UNSORTED_CAP" || NR > maxn || b > maxb) {
            print "CAP\t"
            exit
          }
          print s "\t" $0
        }'
      break
    done
  done
} | {
  _sc_cur_s=
  _sc_cur_i=0
  _sc_tot_c=0
  _sc_tot_b=0
  while IFS="$_bt_tab" read -r s j; do
    if [ "$_sc_bcut" = 1 ]; then break; fi
    if [ "$s" = CAP ]; then _sc_mark_budget; break; fi
    [ -n "$j" ] || continue
    _sc_tot_c=$((_sc_tot_c + 1))
    _sc_tot_b=$((_sc_tot_b + ${#j} + 1))
    if [ "$_sc_tot_c" -gt ` + strconv.Itoa(subCandTotalMax) + ` ] || [ "$_sc_tot_b" -gt ` + strconv.Itoa(subCandTotalBytes) + ` ]; then
      _sc_mark_budget
      break
    fi
    if [ "$s" != "$_sc_cur_s" ]; then _sc_cur_s=$s; _sc_cur_i=0; fi
    [ "$_sc_cur_i" -lt ` + strconv.Itoa(subsScanned) + ` ] || continue
    m=${j%.jsonl}.meta.json
    [ -f "$m" ] || continue
    meta=$(head -c 8192 "$m" 2>/dev/null | tr '\n\t' '  ')
    case $meta in *'"requestShape":"background"'*) ;; *) continue;; esac
    case $meta in *'"finished":true'*) continue;; esac
    _sc_cur_i=$((_sc_cur_i + 1))
    id=${j##*/agent-}; id=${id%.jsonl}
    t=$(stat -c %Y "$m" 2>/dev/null || date -r "$m" +%s 2>/dev/null)
    v=$(head -c 65536 "$j" 2>/dev/null | grep -o '"version":"[0-9.]*"' | head -n 1 | cut -d'"' -f4)
    w=$(printf '%s' "$meta" | grep -oE '"description":"([^"\\]|\\.)*"' | head -n 1 | head -c 600)
    _sc_line=$(printf 'sub\t%s\t%s\t%s\t%s\t{%s}' "$s" "$id" "$t" "$v" "$w")
    if ! _sc_emit "$_sc_line"; then
      # Omitting description text alone still counts as complete; only mark
      # budget cut if even the description-less sub line cannot be emitted.
      _sc_line=$(printf 'sub\t%s\t%s\t%s\t%s\t{}' "$s" "$id" "$t" "$v")
      if ! _sc_emit "$_sc_line"; then
        _sc_mark_budget
        break
      fi
    fi
  done
  exec <&-
  # Priority 3 (lowest): CLI subagent discovery lines within min(cliBudget, _sc_rem).
  if [ "$_bt_budget" -gt "$_sc_rem" ]; then _bt_budget=$_sc_rem; fi
  for _sc_k in $_sc_cmd_pids; do
    [ "$_sc_bcut" = 0 ] || break
    scan_cmd_tree "$_sc_k"
  done
}
exit 0`
	cmd := "sh -c " + sshx.QuoteShell(script) + " corral"
	n := 0
	for _, a := range watched {
		if n >= maxWatchedScanned || (!strings.HasPrefix(a, "w:") && !strings.HasPrefix(a, "s:")) || !convoUUID.MatchString(a[2:]) {
			continue
		}
		cmd += " " + sshx.QuoteShell(a[:2]+strings.ToLower(a[2:]))
		n++
	}
	return cmd
}

// subsScanned: of a session, how many subagents out its look tells at most;
// maxWatchedScanned: how many sessions a look is given.
const (
	subsScanned       = 16
	maxWatchedScanned = 48
)

// parseBgIDs reads the "bgid" lines of what cmdScanCommand printed: of each
// agent session at work, the tasks its log says it set going in the
// background.
func parseBgIDs(out string) map[string]map[string]bool {
	got := map[string]map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[0] != "bgid" || !convoUUID.MatchString(f[1]) || !cmdTaskID.MatchString(f[2]) {
			continue
		}
		s := strings.ToLower(f[1])
		if got[s] == nil {
			got[s] = map[string]bool{}
		}
		got[s][f[2]] = true
	}
	return got
}

var cmdTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// cmdEvalAwk: what follows the first "&& eval '" of a line (Claude Code's
// shell runs the command so, after setting itself up; the command may hold
// another).
const cmdEvalAwk = `{ i = index($0, m); if (i) print substr($0, i + length(m)) }`

// cmdCodex: what a Codex's command's line says in place of where its output
// goes: the thread it was run for.
var cmdCodex = regexp.MustCompile(`^codex:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// cmdAgy: what an Antigravity CLI command's line says in place of where its
// output goes: the conversation UUID it was run for.
var cmdAgy = regexp.MustCompile(`^agy:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// codexSandboxWrapper: a Codex's process that holds a command in its
// sandbox (Linux: codex-linux-sandbox, and bwrap under it; macOS:
// sandbox-exec), the command their child, found as well — 0.167.2,
// sandbox workspace-write, 11-15.
// "outer" for what Codex runs a command in (codex-linux-sandbox,
// sandbox-exec), "inner" for what that runs it in (bwrap), "" for neither.
func codexSandboxWrapper(cmdline string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(cmdline), " ")
	switch first[strings.LastIndex(first, "/")+1:] {
	case "codex-linux-sandbox", "sandbox-exec":
		return "outer"
	case "bwrap":
		return "inner"
	}
	return ""
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
	cmds, whole, _ = parseCmdScanDetail(out)
	return cmds, whole
}

// parseCmdScanDetail additionally reports budgetCut when the script stopped
// emitting plain commands due to hitting its unified output byte budget
// ("budget" marker), so cmdProber.run can retain previous lists instead of
// clearing them.
func parseCmdScanDetail(out string) (cmds map[string][]bgCmd, whole bool, budgetCut bool) {
	whole = true
	var btime, hz int64
	type found struct {
		pid     string
		session string
		cmd     bgCmd
		start   int64
	}
	type rawCLI struct {
		cmdPID  string
		procPID string
		comm    string
		exe     string
		sid     string
		log     string
		argv    string
	}
	var all, wrappers []found
	var rawCLIs []rawCLI
	anc := map[string][]string{} // (a Codex's command's process → its parents, nearest first)
	desc := map[string]string{}  // (session + " " + task → what the agent said it does)
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 5)
		switch {
		case line == "more":
			whole = false
		case line == "budget":
			whole = false
			budgetCut = true
		case len(f) == 3 && f[0] == "anc" && cmdPID.MatchString(f[1]):
			for _, a := range strings.Split(f[2], ",") {
				if cmdPID.MatchString(a) {
					anc[f[1]] = append(anc[f[1]], a)
				}
			}
		case len(f) == 4 && f[0] == "desc" && convoUUID.MatchString(f[1]) && cmdTaskID.MatchString(f[2]):
			var d struct {
				Description string `json:"description"`
			}
			if json.Unmarshal([]byte(f[3]), &d) == nil {
				if w := cmdTitle(d.Description); w != "" {
					desc[strings.ToLower(f[1])+" "+f[2]] = w
				}
			}
		case len(f) == 3 && f[0] == "clock":
			btime, _ = strconv.ParseInt(f[1], 10, 64)
			hz, _ = strconv.ParseInt(f[2], 10, 64)
		case strings.HasPrefix(line, "cli\t"):
			cf := strings.SplitN(line, "\t", 8)
			if len(cf) == 8 && cmdPID.MatchString(cf[1]) && cmdPID.MatchString(cf[2]) {
				rawCLIs = append(rawCLIs, rawCLI{
					cmdPID:  cf[1],
					procPID: cf[2],
					comm:    cf[3],
					exe:     cf[4],
					sid:     strings.ToLower(strings.TrimSpace(cf[5])),
					log:     strings.TrimSpace(cf[6]),
					argv:    cf[7],
				})
			}
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
				switch codexSandboxWrapper(f[4]) {
				case "outer":
					wrappers = append(wrappers, found{pid: f[1], session: strings.ToLower(m[1]), cmd: bgCmd{Task: "codex-" + f[1]}, start: start})
					continue // (see below)
				case "inner":
					continue // (bwrap, under codex-linux-sandbox: that one stands for it)
				}
				session, task = m[1], "codex-"+f[1]
			} else if m := cmdAgy.FindStringSubmatch(f[3]); m != nil && cmdPID.MatchString(f[1]) {
				session, task = m[1], "agy-"+f[1]
			} else {
				continue
			}
			mCmd, mCut := cmdMatchText(f[4])
			all = append(all, found{pid: f[1], session: strings.ToLower(session), cmd: bgCmd{Task: task, Title: cmdTitle(f[4]), Out: out, quiet: strings.HasPrefix(task, "monitor-"), matchCmd: mCmd, matchCut: mCut}, start: start})
		}
	}
	if btime <= 0 {
		whole = false // (no /proc: no process of any agent found, which is not none running)
	}
	// (A Codex's sandbox around a command — codex-linux-sandbox, bwrap —:
	// the command itself is found too where what runs it under the sandbox
	// is a Codex as well (0.167.2 on Linux, 11-15): then one command, not
	// two. Where none of its thread is found under it, the wrapper stands
	// for one, its command not told (cut short among its arguments): a
	// command running all the same — review r2a-9.)
	// (Its own command: one of its thread whose parents it is among —
	// not one begun after it by its time alone: review r2b-1.)
	under := map[string]bool{}
	for _, c := range all {
		pid := strings.TrimPrefix(c.cmd.Task, "codex-")
		for _, a := range anc[pid] {
			under[c.session+" "+a] = true
		}
	}
	for _, w := range wrappers {
		if !under[w.session+" "+strings.TrimPrefix(w.cmd.Task, "codex-")] {
			all = append(all, w)
		}
	}
	// Index verified CLI subagents by the background command PID that spawned them.
	clisByPID := map[string][]bgCLI{}
	for _, rc := range rawCLIs {
		agent := identifyBgCLI(rc.comm, rc.exe, rc.argv)
		if agent == "" || !bgCLILogValid(agent, rc.log, rc.sid) {
			continue
		}
		cli := bgCLI{Agent: agent, Session: rc.sid, Log: rc.log}
		if !slices.Contains(clisByPID[rc.cmdPID], cli) {
			clisByPID[rc.cmdPID] = append(clisByPID[rc.cmdPID], cli)
		}
	}
	slices.SortStableFunc(all, func(a, b found) int { return int(min(max(a.start-b.start, -1), 1)) })
	got := map[string][]bgCmd{}
	for _, c := range all {
		var attached []bgCLI
		for _, cli := range clisByPID[c.pid] {
			if strings.EqualFold(cli.Session, c.session) {
				continue // never alias the parent session onto itself
			}
			attached = append(attached, cli)
		}
		if idx := slices.IndexFunc(got[c.session], func(o bgCmd) bool { return o.Task == c.cmd.Task }); idx >= 0 {
			// Merge any CLI subagents discovered on sibling/child processes of the same task.
			for _, cli := range attached {
				if !slices.Contains(got[c.session][idx].clis, cli) {
					got[c.session][idx].clis = append(got[c.session][idx].clis, cli)
				}
			}
			continue // (a process of a task already told: its shell's child)
		}
		if btime > 0 && hz > 0 && hz <= 100000 && c.start < (1<<62)/1000 {
			c.cmd.Since = btime*1000 + c.start*1000/hz
		}
		c.cmd.Desc = desc[c.session+" "+c.cmd.Task]
		c.cmd.clis = attached
		got[c.session] = append(got[c.session], c.cmd)
	}
	return got, whole, budgetCut
}

// identifyBgCLI strictly identifies whether a process inside a background
// command tree is a known agent CLI ("agy", "claude", "codex") running an
// agent session. Returns "" for non-agent binaries (even if their arguments
// mention "claude", e.g. `grep claude` or `cat claude.md`), utility
// subcommands (help/login/mcp/app-server), or ephemeral/no-persistence runs.
func identifyBgCLI(comm, exe, argvRaw string) string {
	comm = strings.TrimSpace(comm)
	exe = strings.TrimSuffix(strings.TrimSpace(exe), " (deleted)")
	exeBase := ""
	if exe != "" {
		exeBase = path.Base(exe)
	}
	var argv []string
	if argvRaw != "" {
		argv = strings.Split(argvRaw, "\x01")
		for len(argv) > 0 && argv[len(argv)-1] == "" {
			argv = argv[:len(argv)-1]
		}
	}
	argv0Base := ""
	if len(argv) > 0 && argv[0] != "" {
		argv0Base = path.Base(argv[0])
	}

	kind := ""
	argStart := 1
	switch exeBase {
	case "agy":
		kind = "agy"
	case "claude":
		kind = "claude"
	case "codex":
		kind = "codex"
	case "node", "nodejs", "bun", "deno":
		script, nextIdx := nodeScriptArg(argv)
		if script == "" {
			return ""
		}
		argStart = nextIdx
		sb := path.Base(script)
		switch {
		case strings.Contains(script, "claude-code/cli") || sb == "claude":
			kind = "claude"
		case strings.Contains(script, "@openai/codex/bin/codex") || sb == "codex":
			kind = "codex"
		case strings.Contains(script, "antigravity-cli/") || sb == "agy":
			kind = "agy"
		default:
			return ""
		}
	case "sh", "dash", "bash", "zsh":
		// A #!/bin/sh wrapper executable named agy/claude/codex sets comm to the
		// script's basename and argv to [sh, /path/to/<name>, ...]. Never match `-c`.
		script, nextIdx := shellScriptArg(argv)
		if script == "" {
			return ""
		}
		sb := path.Base(script)
		if (sb == "agy" || sb == "claude" || sb == "codex") && comm == sb {
			kind = sb
			argStart = nextIdx
		} else {
			return ""
		}
	default:
		if strings.Contains(exe, "/claude/versions/") {
			kind = "claude"
		} else if exe == "" {
			// Fallback only when /proc/<pid>/exe was unavailable: require comm AND argv[0] to agree.
			switch comm {
			case "agy", "claude", "codex":
				if argv0Base == "" || argv0Base == comm {
					kind = comm
				}
			}
		}
	}
	if kind == "" {
		return ""
	}
	args := []string{}
	if argStart < len(argv) {
		args = argv[argStart:]
	}
	if !bgCLIArgsEligible(kind, args) {
		return ""
	}
	return kind
}

// shellScriptArg returns the script path executed directly by a POSIX shell
// (e.g. `#!/bin/sh` wrapper `/usr/local/bin/agy`) and the index of the first
// argument passed to that script. Returns ("", 0) for `-c` command strings or
// when no script file is executed.
func shellScriptArg(argv []string) (string, int) {
	if len(argv) < 2 {
		return "", 0
	}
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			if i+1 < len(argv) {
				return argv[i+1], i + 2
			}
			return "", 0
		}
		if a == "-c" || a == "-s" {
			return "", 0
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			if strings.ContainsAny(a[1:], "cs") {
				return "", 0
			}
			continue
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		return a, i + 1
	}
	return "", 0
}

// nodeScriptArg returns the script path executed by node/bun/deno and the
// index of the first argument passed to that script. Returns ("", 0) for
// inline eval (-e/-p, including attached forms like --eval=code and -ecode)
// or when no script path is present.
func nodeScriptArg(argv []string) (string, int) {
	if len(argv) < 2 {
		return "", 0
	}
	skipNext := false
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--" {
			if i+1 < len(argv) {
				return argv[i+1], i + 2
			}
			return "", 0
		}
		if a == "-e" || a == "--eval" || strings.HasPrefix(a, "--eval=") ||
			a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=") ||
			a == "-c" || a == "--check" ||
			(len(a) > 2 && a[0] == '-' && a[1] != '-' && (a[1] == 'e' || a[1] == 'p')) {
			return "", 0
		}
		switch a {
		case "-r", "--require", "--import", "--loader", "--experimental-loader",
			"-C", "--conditions", "--env-file", "--max-old-space-size",
			"--inspect", "--inspect-brk", "--title", "--redirect-warnings",
			"--diagnostic-dir", "--heapsnapshot-signal", "--input-type",
			"--stack-trace-limit", "--test-name-pattern":
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, i + 1
	}
	return "", 0
}

// bgCLIArgsEligible rejects --help/--version, ephemeral/no-persistence flags,
// and utility subcommands (mcp, login, app-server, etc.) while allowing print/
// exec/interactive agent runs.
func bgCLIArgsEligible(kind string, args []string) bool {
	inPromptMode := false
	firstSub := ""
	skipVal := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if skipVal {
			skipVal = false
			continue
		}
		switch a {
		case "-h", "--help", "--version", "-V":
			return false
		}
		switch kind {
		case "claude":
			switch {
			case a == "-v" || a == "--no-session-persistence":
				return false
			case a == "-p" || a == "--print" || strings.HasPrefix(a, "--print="):
				inPromptMode = true
				continue
			case a == "--model" || a == "--output-format" || a == "--input-format" ||
				a == "--resume" || a == "-r" || a == "--session-id" || a == "--add-dir" ||
				a == "--allowedTools" || a == "--disallowedTools" || a == "--permission-mode" ||
				a == "--append-system-prompt" || a == "--system-prompt" || a == "--max-turns" ||
				a == "--mcp-config" || a == "--settings" || a == "--agent" || a == "--effort" ||
				a == "--fallback-model" || a == "--json-schema":
				skipVal = true
				continue
			}
		case "agy":
			switch {
			case a == "-p" || a == "--print" || a == "--prompt" || strings.HasPrefix(a, "--print=") || strings.HasPrefix(a, "--prompt="):
				inPromptMode = true
				continue
			case a == "-i" || a == "--prompt-interactive" || a == "--add-dir" || a == "--agent" ||
				a == "--conversation" || a == "--effort" || a == "--input-format" || a == "--json-schema" ||
				a == "--log-file" || a == "--mode" || a == "--model" || a == "--output-format" ||
				a == "--print-timeout" || a == "--project" || a == "--settings":
				skipVal = true
				continue
			}
		case "codex":
			switch {
			case a == "--ephemeral":
				return false
			case a == "-c" || a == "--config" || a == "-m" || a == "--model" ||
				a == "-p" || a == "--profile" || a == "-C" || a == "--cd" ||
				a == "-s" || a == "--sandbox" || a == "-a" || a == "--ask-for-approval" ||
				a == "-i" || a == "--image" || a == "-o" || a == "--output-last-message" ||
				a == "--output-schema" || a == "--add-dir" || a == "--color" || a == "--local-provider":
				skipVal = true
				continue
			}
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if firstSub == "" && !inPromptMode {
			firstSub = a
		}
	}
	switch kind {
	case "agy":
		switch firstSub {
		case "acp", "agent", "agents", "changelog", "help", "install", "mcp", "mic-serve", "models", "plugin", "plugins", "remote-control", "update":
			return false
		}
	case "claude":
		switch firstSub {
		case "mcp", "config", "doctor", "install", "update", "migrate-installer", "setup-token", "auth", "login", "logout", "plugin":
			return false
		}
	case "codex":
		switch firstSub {
		case "app-server", "mcp", "mcp-server", "login", "logout", "completion", "sandbox", "debug", "features", "apply", "a", "help", "cloud":
			return false
		}
	}
	return true
}

// bgCLILogValid verifies that log is a clean absolute path matching the
// expected per-session JSONL layout for agent ("agy", "claude", "codex") and
// session UUID sid.
func bgCLILogValid(agent, log, sid string) bool {
	sid = strings.ToLower(strings.TrimSpace(sid))
	if !convoUUID.MatchString(sid) || sid == convoNoSession || strings.ContainsAny(log, "\t\r\n") {
		return false
	}
	l, ok := cleanRemotePath(log)
	if !ok {
		return false
	}
	switch agent {
	case "agy":
		return strings.HasSuffix(l, "/brain/"+sid+"/.system_generated/logs/transcript_full.jsonl")
	case "codex":
		return strings.Contains(l, "/sessions/") && strings.EqualFold(convoSessionOf(l), sid)
	case "claude":
		return strings.Contains(l, "/projects/") && !strings.Contains(l, "/subagents/") && strings.HasSuffix(strings.ToLower(l), "/"+sid+".jsonl")
	default:
		return false
	}
}

// splitBgCmds partitions background commands into plain commands (no resolved
// agent CLI, or synthetic task whose command exceeded maxBgCmdMatchBytes so it
// cannot be safely matched without risking duplicate plain+CLI display) and
// promoted CLI subagents (one per resolved CLI session).
func splitBgCmds(cmds []bgCmd) (plain []bgCmd, cliAgents []bgAgent) {
	seen := map[string]bool{}
	for _, c := range cmds {
		if len(c.clis) == 0 || (isSyntheticBgTask(c.Task) && (c.matchCut || (c.matchCmd == "" && c.Title == ""))) {
			plain = append(plain, c)
			continue
		}
		what := c.Desc
		if what == "" {
			what = c.Title
		}
		for _, cli := range c.clis {
			k := cli.Agent + "\x00" + cli.Session
			if seen[k] {
				continue
			}
			seen[k] = true
			cliAgents = append(cliAgents, bgAgent{
				ID:       cli.Session,
				What:     what,
				Since:    c.Since,
				Agent:    cli.Agent,
				Task:     c.Task,
				Cmd:      c.Title,
				log:      cli.Log,
				matchCmd: c.matchCmd,
				matchCut: c.matchCut,
			})
		}
	}
	return plain, cliAgents
}

func bgCmdTaskIDs(cmds []bgCmd) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if c.Task != "" {
			out = append(out, c.Task)
		}
	}
	return out
}

func isSyntheticBgTask(task string) bool {
	return strings.HasPrefix(task, "codex-") || strings.HasPrefix(task, "agy-") || strings.HasPrefix(task, "monitor-")
}

func normBgCmdText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func exactBgCmdMatch(r convo.Running, grp []bgAgent) bool {
	if grp[0].matchCut || r.CmdCut {
		return false
	}
	cmdText := normBgCmdText(grp[0].matchCmd)
	if cmdText == "" {
		cmdText = normBgCmdText(grp[0].Cmd)
	}
	if cmdText == "" || len(cmdText) > maxBgCmdMatchBytes {
		return false
	}
	// Require the log-side full command (r.Cmd); never fall back to the 300-byte
	// truncated display summary r.What.
	rText := normBgCmdText(r.Cmd)
	if rText == "" || len(rText) > maxBgCmdMatchBytes {
		return false
	}
	return rText == cmdText
}

// upgradeRunningWithCLIs upgrades any log-tracked background command in running
// whose task ID (or, for Codex/agy/Monitor synthetic host tasks, whose exact
// normalized command text uniquely matches 1-to-1) matches a host-discovered
// background CLI subagent into a kind="agent" entry carrying the CLI kind,
// session ID, and host background task ID.
func upgradeRunningWithCLIs(running []convo.Running, cliAgents []bgAgent) []convo.Running {
	out, _ := reconcileRunningWithCLIs(running, cliAgents)
	return out
}

// reconcileRunningWithCLIs upgrades matching running commands to CLI subagents
// AND filters cliAgents to avoid emitting unmatched synthetic CLI entries
// alongside unupgraded opaque/truncated or ambiguous plain commands (e.g.
// Codex code-mode multi-command calls where log-side Cmd is empty and CmdCut
// is true).
func reconcileRunningWithCLIs(running []convo.Running, cliAgents []bgAgent) ([]convo.Running, []bgAgent) {
	if len(cliAgents) == 0 {
		return running, nil
	}
	if len(running) == 0 {
		return running, cliAgents
	}
	byTask := map[string][]bgAgent{}
	var synthOrder []string
	synthByTask := map[string][]bgAgent{}
	for _, a := range cliAgents {
		if a.Agent == "" || a.Task == "" {
			continue
		}
		byTask[a.Task] = append(byTask[a.Task], a)
		if isSyntheticBgTask(a.Task) {
			if _, ok := synthByTask[a.Task]; !ok {
				synthOrder = append(synthOrder, a.Task)
			}
			synthByTask[a.Task] = append(synthByTask[a.Task], a)
		}
	}
	if len(byTask) == 0 {
		return running, cliAgents
	}

	// Pre-compute 1-to-1 exact-text matches for synthetic host tasks so we never
	// upgrade on substring containment or ambiguous multi-candidate matches.
	matchedByIdx := map[int][]bgAgent{}
	matchedTasks := map[string]bool{}
	runCands := map[int][]string{}
	synthCands := map[string][]int{}
	for i, r := range running {
		if r.Kind != "cmd" {
			continue
		}
		if grp := byTask[r.ID]; len(grp) > 0 {
			matchedByIdx[i] = grp
			matchedTasks[r.ID] = true
			continue
		}
		if len(synthOrder) == 0 {
			continue
		}
		for _, task := range synthOrder {
			if exactBgCmdMatch(r, synthByTask[task]) {
				runCands[i] = append(runCands[i], task)
				synthCands[task] = append(synthCands[task], i)
			}
		}
	}
	for i, tasks := range runCands {
		if len(tasks) == 1 && len(synthCands[tasks[0]]) == 1 {
			matchedByIdx[i] = synthByTask[tasks[0]]
			matchedTasks[tasks[0]] = true
		}
	}

	hasOpaqueCmd := false
	out := make([]convo.Running, 0, len(running))
	for i, r := range running {
		if r.Kind != "cmd" {
			out = append(out, r)
			continue
		}
		matched := matchedByIdx[i]
		if len(matched) == 0 {
			if r.CmdCut || normBgCmdText(r.Cmd) == "" {
				hasOpaqueCmd = true
			}
			out = append(out, r)
			continue
		}
		for _, a := range matched {
			what := r.What
			if what == "" {
				what = a.What
			}
			if what == "" {
				what = r.Cmd
			}
			ts := r.Time
			if ts == "" && a.Since > 0 {
				ts = time.UnixMilli(a.Since).UTC().Format(time.RFC3339)
			}
			taskID := a.Task
			if taskID == "" {
				taskID = r.ID
			}
			out = append(out, convo.Running{
				ID:    a.ID,
				What:  what,
				Kind:  "agent",
				Time:  ts,
				Cmd:   r.Cmd,
				Out:   r.Out,
				Agent: a.Agent,
				Task:  taskID,
			})
		}
	}

	var keptCLIs []bgAgent
	for _, a := range cliAgents {
		if isSyntheticBgTask(a.Task) && !matchedTasks[a.Task] && (hasOpaqueCmd || len(synthCands[a.Task]) > 0) {
			continue
		}
		keptCLIs = append(keptCLIs, a)
	}
	return out, keptCLIs
}

func cleanBgCmdText(s string) string {
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
	return s
}

// cmdMatchText returns the cleaned full command for exact synthetic task
// matching, or ("", true) when the command exceeds maxBgCmdMatchBytes so
// truncated commands never participate in upgrade matching.
func cmdMatchText(s string) (string, bool) {
	if len(s) > maxBgCmdMatchBytes {
		return "", true
	}
	cleaned := cleanBgCmdText(s)
	if len(cleaned) > maxBgCmdMatchBytes {
		return "", true
	}
	return cleaned, false
}

// cmdTitle: what a command runs, from its shell's arguments from eval on
// ("'<command>' < /dev/null && pwd -P >| …" as Claude Code words it; of
// a long one, its start), on one line; "" when it cannot be told.
func cmdTitle(s string) string {
	return truncate(cleanBgCmdText(s), 80)
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
