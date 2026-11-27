package web

import (
	"context"
	"encoding/json"
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
	maxCmdsShown = 8         // listed per agent (all are counted; the card lists each, 11-15)
	cmdScanMax   = 512 << 10 // (with the subagents out and the background ids of those at work: review of 11-25)
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
	// Desc: what the agent said the command does (a Claude Code call's
	// description), where its log tells it; the page names it so.
	Desc string `json:"desc,omitempty"`
	// quiet: a Monitor's — in the background by its nature, whatever the
	// agent's log says (see bgTaskIDs). (Not sent.)
	quiet bool
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
	at      time.Time            // when they were found (zero: not known)
	soon    time.Time            // a look due soon (a pane came to done or idle)
	wake    chan struct{}
	poke    func()
	scan func(ctx context.Context, watched []string) (cmds map[string][]bgCmd, subs map[string][]bgAgent, bgIDs map[string]map[string]bool, whole bool, err error)
	// logOf resolves the confirmed full Claude log path for (pane, sid), if known.
	logOf func(pane, sid string) string
	// paneKeys records the watch key ("<sid>" or "<sid>=<logPath>") used for
	// each pane in the latest completed scan, so two panes running the same
	// sid from different directories do not overwrite each other's subagents/bgIDs.
	paneKeys map[string]string
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

func (p *cmdProber) setLogOf(f func(pane, sid string) string) {
	p.mu.Lock()
	p.logOf = f
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
			// (At work: those it set going in the background only — not what it runs in front now.)
			if e, ok := p.working[a.PaneID]; ok && (a.Session == "" || strings.ToLower(a.Session) == e.session) && !p.at.Before(e.from) {
				key := p.paneWatchKeyLocked(a.PaneID, e.session)
				var quiet []bgCmd
				for _, c := range p.cmds[e.session] {
					if c.quiet || p.bgIDs[key][c.Task] {
						quiet = append(quiet, c)
					}
				}
				if len(quiet) > 0 {
					a.BgCmds, a.BgCmdList = len(quiet), quiet[:min(len(quiet), maxCmdsShown)]
				}
			}
			if sess := p.paneSessionLocked(a.PaneID, a.Session); sess != "" {
				key := p.paneWatchKeyLocked(a.PaneID, sess)
				a.BgAgentList = subsShown(p.subs[key], a.Status, a.Bg, a.BgN)
			}
		}
	}
}

func (p *cmdProber) paneWatchKeyLocked(pane, sid string) string {
	if k := p.paneKeys[pane]; k != "" {
		if k == sid || strings.HasPrefix(k, sid+"=") {
			return k
		}
	}
	return sid
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
		workSnap := make(map[string]string, len(p.working))
		for pane, e := range p.working {
			workSnap[pane] = e.session
		}
		endSnap := make(map[string]string, len(p.ended))
		for pane, e := range p.ended {
			endSnap[pane] = e.session
		}
		logOf := p.logOf
		if !p.soon.After(began) {
			p.soon = time.Time{} // (else it is due still: one coming to done or idle since the last)
		}
		p.mu.Unlock()
		if !due {
			continue
		}
		// (The sessions it is for: at work, "w:", their background commands told apart; at rest, "s:".)
		paneKeys := make(map[string]string, len(workSnap)+len(endSnap))
		var watched []string
		for pane, sid := range workSnap {
			key := sid
			if logOf != nil {
				if k := claudeWatchKey(sid, logOf(pane, sid)); k != "" {
					key = k
				}
			}
			paneKeys[pane] = key
			if !slices.Contains(watched, "w:"+key) {
				watched = append(watched, "w:"+key)
			}
		}
		for pane, sid := range endSnap {
			key := sid
			if logOf != nil {
				if k := claudeWatchKey(sid, logOf(pane, sid)); k != "" {
					key = k
				}
			}
			paneKeys[pane] = key
			if !slices.Contains(watched, "s:"+key) && !slices.Contains(watched, "w:"+key) {
				watched = append(watched, "s:"+key)
			}
		}
		last = began
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		found, subs, ids, whole, err := p.scan(sctx, watched)
		cancel()
		p.mu.Lock()
		// (What this look found, in place of the last's: one ended, or of a session no longer at
		// work, goes with it — review of 11-25.)
		if ids == nil {
			ids = map[string]map[string]bool{}
		}
		changed := !sameIDs(p.bgIDs, ids)
		p.bgIDs = ids
		if err != nil || !whole {
			// (Not known, or not all of it: nothing told, rather than what was.)
			changed = changed || !p.at.IsZero()
			p.at, p.cmds, p.subs, p.paneKeys = time.Time{}, nil, nil, nil
		} else {
			changed = changed || p.at.IsZero() || !sameCmds(p.cmds, found) || !sameSubs(p.subs, subs)
			// (Or the same, but now told of a pane it was not before: one
			// come to done or idle after the last look began.)
			for pane, e := range p.ended {
				k := paneKeys[pane]
				if k == "" {
					k = e.session
				}
				if p.at.Before(e.from) && !began.Before(e.from) && (len(found[e.session]) > 0 || len(subs[k]) > 0) {
					changed = true
				}
			}
			for pane, e := range p.working {
				k := paneKeys[pane]
				if k == "" {
					k = e.session
				}
				if p.at.Before(e.from) && !began.Before(e.from) && (len(found[e.session]) > 0 || len(subs[k]) > 0) {
					changed = true
				}
			}
			p.at, p.cmds, p.subs, p.paneKeys = began, found, subs, paneKeys // (as of when the look began)
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
func cmdScanCommand(watched ...string) string {
	script := claudeConfigDirsSh + `btime=$(sed -n 's/^btime \([0-9]*\)$/\1/p' /proc/stat 2>/dev/null); hz=$(getconf CLK_TCK 2>/dev/null)
printf 'clock\t%s\t%s\n' "$btime" "$hz"
_cldirs_loaded=0
_ensure_cldirs() {
  if [ "$_cldirs_loaded" = 0 ]; then claude_cfg_dirs; _cldirs_loaded=1; fi
}
_pnl=$(printf '\nx'); _pnl=${_pnl%x}
_pins=
for a in "$@"; do
  _rest=${a#?:}
  case $_rest in
  *=*) _pins="${_pins}${_rest%%=*}	${_rest#*=}$_pnl" ;;
  esac
done
_pin_log_for_sid() {
  _pin= _pin_ambig=0
  [ -n "$_pins" ] || return 0
  while IFS='	' read -r _ps _pl; do
    [ "$_ps" = "$1" ] || continue
    if [ -z "$_pin" ]; then
      _pin=$_pl
    elif [ "$_pin" != "$_pl" ]; then
      _pin=
      _pin_ambig=1
      break
    fi
  done <<CORRAL_PINS
$_pins
CORRAL_PINS
}
# _sole_claude_log <sid>: sets _best to the sole matching log across $cldirs
# and _lfound to the match count (0, 1, or 2 for ambiguous). Never guesses by mtime.
_sole_claude_log() {
  _best= _lfound=0
  _ensure_cldirs
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
# _resolve_log <sid> <pinnedLog>: uses exact pinned log when provided; otherwise
# uses the sole unambiguous log across $cldirs (empty when absent or ambiguous).
_resolve_log() {
  _best= _lfound=0
  if [ -n "$2" ]; then
    if [ -f "$2" ]; then _best=$2; _lfound=1; fi
    return 0
  fi
  _sole_claude_log "$1"
}
# _resolve_subs <sid> <pinnedLog>: derives subagents strictly from the confirmed
# or sole log (<log without .jsonl>/subagents). Never searches across projects or
# config dirs when a log is pinned or found; when unpinned and no log exists at
# all, accepts only a sole unambiguous subagents directory.
_resolve_subs() {
  _bestd=
  if [ -n "$2" ]; then
    if [ -d "${2%.jsonl}/subagents" ]; then _bestd="${2%.jsonl}/subagents"; fi
    return 0
  fi
  _sole_claude_log "$1"
  if [ "$_lfound" -eq 1 ]; then
    if [ -d "${_best%.jsonl}/subagents" ]; then _bestd="${_best%.jsonl}/subagents"; fi
    return 0
  fi
  if [ "$_lfound" -gt 1 ]; then
    return 0
  fi
  _dfound=0
  while IFS= read -r _cd; do
    [ -n "$_cd" ] || continue
    for _d in "$_cd"/projects/*/"$1"/subagents; do
      [ -d "$_d" ] || continue
      _dfound=$((_dfound + 1))
      if [ "$_dfound" -eq 1 ]; then
        _bestd=$_d
      else
        _bestd=
        break
      fi
    done
    [ "$_dfound" -le 1 ] || break
  done <<CORRAL_CLDIRS
$cldirs
CORRAL_CLDIRS
}
# (Of each session watched — "w:<id>" at work, "s:<id>" at rest, optionally
# with "=<confirmedLogPath>" when locate already confirmed its exact log —,
# its subagents sent off in the background whose log was written to in the last
# six hours, not marked finished: what its meta says it was sent for. At most ` + strconv.Itoa(subsScanned) + ` a session.)
for a in "$@"; do
  k=${a#?:}
  case $k in
  *=*) s=${k%%=*}; _ap=${k#*=} ;;
  *) s=$k; _ap= ;;
  esac
  _resolve_subs "$s" "$_ap"
  d=$_bestd
  [ -d "$d" ] || continue
  find "$d" -maxdepth 1 -name 'agent-*.jsonl' -mmin -360 2>/dev/null | {
    i=0
    while IFS= read -r j; do
      m=${j%.jsonl}.meta.json
      [ -f "$m" ] || continue
      meta=$(head -c 8192 "$m" 2>/dev/null | tr '\n\t' '  ')
      case $meta in *'"requestShape":"background"'*) ;; *) continue;; esac
      case $meta in *'"finished":true'*) continue;; esac
      [ "$i" -lt ` + strconv.Itoa(subsScanned) + ` ] || break
      i=$((i + 1))
      id=${j##*/agent-}; id=${id%.jsonl}
      t=$(stat -c %Y "$m" 2>/dev/null || date -r "$m" +%s 2>/dev/null)
      v=$(head -c 65536 "$j" 2>/dev/null | grep -o '"version":"[0-9.]*"' | head -n 1 | cut -d'"' -f4)
      w=$(printf '%s' "$meta" | grep -oE '"description":"([^"\\]|\\.)*"' | head -n 1 | head -c 600)
      printf 'sub\t%s\t%s\t%s\t%s\t{%s}\n' "$k" "$id" "$t" "$v" "$w"
    done
  }
done
seen=
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
  codex:*|agy:*) t=$(tr '\0\n\t' '   ' <"/proc/$k/cmdline" 2>/dev/null | sed 's/^[^ ]*sh -l\{0,1\}c //' | head -c 400);; # (a shell's command; else the program itself, run as it is)
  *) t=$(tr '\0\n\t' '   ' <"/proc/$k/cmdline" 2>/dev/null | awk -v "m=&& eval '" '` + cmdEvalAwk + `' | head -c 400);;
  esac
  printf 'cmd\t%s\t%s\t%s\t%s\n' "$k" "$st" "$(printf '%s' "$l" | tr '\t\n' '  ')" "$t"
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
    printf 'anc\t%s\t%s\n' "$k" "${anc#,}";;
  esac
done
# (Of a session at work, which of the commands found running its log says it set going in the
# background — the whole log: one begun an hour ago counts; a command run in front writes to a
# task's file too, and is not one. A grep of a log of 200 MB: some 50 ms.)
for a in "$@"; do
  case $a in w:*) ;; *) continue;; esac
  [ -n "$seen" ] || break
  k=${a#w:}
  case $k in
  *=*) s=${k%%=*}; _ap=${k#*=} ;;
  *) s=$k; _ap= ;;
  esac
  _resolve_log "$s" "$_ap"
  f=$_best
  [ -f "$f" ] || continue
  grep -o '"backgroundTaskId":"[A-Za-z0-9_-]*"' "$f" | sed 's/^"backgroundTaskId":"//; s/"$//' | sort -u | while IFS= read -r id; do
    case " $seen " in *" $id "*) printf 'bgid\t%s\t%s\n' "$k" "$id";; esac
  done
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
  _pin_log_for_sid "$s"
  [ "$_pin_ambig" = 0 ] || continue
  _resolve_log "$s" "$_pin"
  f=$_best
  [ -f "$f" ] || continue
  i=$((i + 1))
  tu=$(grep -m 1 -F "\"backgroundTaskId\":\"$id\"" "$f" | grep -o '"tool_use_id":"[A-Za-z0-9_-]*"' | head -n 1 | cut -d'"' -f4)
  [ -n "$tu" ] || continue
  w=$(grep -m 1 -F "\"id\":\"$tu\"" "$f" | sed "s/^.*\"id\":\"$tu\"//; s/\"id\":\".*//; s/\"type\":\"tool_use\".*//" | grep -oE '"description":"([^"\\]|\\.)*"' | head -n 1 | head -c 600)
  [ -n "$w" ] && printf 'desc\t%s\t%s\t{%s}\n' "$s" "$id" "$w"
done
exit 0`
	cmd := "sh -c " + sshx.QuoteShell(script) + " corral"
	n := 0
	for _, a := range watched {
		if n >= maxWatchedScanned || (!strings.HasPrefix(a, "w:") && !strings.HasPrefix(a, "s:")) {
			continue
		}
		prefix := a[:2]
		if key, _, _, ok := parseClaudeWatchKey(a[2:]); ok {
			cmd += " " + sshx.QuoteShell(prefix+key)
			n++
		}
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
		if len(f) != 3 || f[0] != "bgid" || !cmdTaskID.MatchString(f[2]) {
			continue
		}
		key, _, _, ok := parseClaudeWatchKey(f[1])
		if !ok {
			continue
		}
		if got[key] == nil {
			got[key] = map[string]bool{}
		}
		got[key][f[2]] = true
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
	whole = true
	var btime, hz int64
	type found struct {
		session string
		cmd     bgCmd
		start   int64
	}
	var all, wrappers []found
	anc := map[string][]string{} // (a Codex's command's process → its parents, nearest first)
	desc := map[string]string{}  // (session + " " + task → what the agent said it does)
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 5)
		switch {
		case line == "more":
			whole = false
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
					wrappers = append(wrappers, found{session: strings.ToLower(m[1]), cmd: bgCmd{Task: "codex-" + f[1]}, start: start})
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
			all = append(all, found{session: strings.ToLower(session), cmd: bgCmd{Task: task, Title: cmdTitle(f[4]), Out: out, quiet: strings.HasPrefix(task, "monitor-")}, start: start})
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
	slices.SortStableFunc(all, func(a, b found) int { return int(min(max(a.start-b.start, -1), 1)) })
	got := map[string][]bgCmd{}
	for _, c := range all {
		if slices.ContainsFunc(got[c.session], func(o bgCmd) bool { return o.Task == c.cmd.Task }) {
			continue // (a process of a task already told: its shell's child)
		}
		if btime > 0 && hz > 0 && hz <= 100000 && c.start < (1<<62)/1000 {
			c.cmd.Since = btime*1000 + c.start*1000/hz
		}
		c.cmd.Desc = desc[c.session+" "+c.cmd.Task]
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
