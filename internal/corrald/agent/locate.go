//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	// LocateScopeFull identifies a full host scan (only == "").
	LocateScopeFull = "full"
	// LocateScopeOpenCode identifies an opencode-only scan (only == "opencode").
	LocateScopeOpenCode = "opencode"
)

// LocatorOptions configures a Locator instance. Zero values default to the
// current user's host environment (/proc, os.UserHomeDir, os.Geteuid).
type LocatorOptions struct {
	Home     string
	UID      *uint32
	ProcRoot string
	// ListPIDs optionally overrides /proc PID enumeration (returning numeric
	// PID strings). When nil (production default), Locator and Watcher read
	// numeric entries from ProcRoot (default "/proc").
	ListPIDs        func() ([]string, error)
	Tmux            *TmuxExecutor
	ClaudeConfigDir *string // override for /proc/self/environ CLAUDE_CONFIG_DIR
	XDGDataHome     *string // override for /proc/self/environ XDG_DATA_HOME
	Now             func() time.Time
	OnDiscovered    func(scope string, logs []string, claudeCfgDirs []string, codexHomes []string)
	OnHerdrSockets  func(scope string, sockets []string)
}

// Locator implements the host-side agent discovery logic matching Corral's
// convoLocateWith, convoLocateOpenCodeCommand, convoLocateTimesCommand, and
// convoLocatePidsCommand scripts byte-for-byte.
type Locator struct {
	home                    string
	uid                     uint32
	procRoot                string
	listPIDs                func() ([]string, error)
	rd                      *capReader
	tmux                    *TmuxExecutor
	claudeConfigDirOverride *string
	xdgDataHomeOverride     *string
	nowFn                   func() time.Time
	onDiscovered            func(scope string, logs []string, claudeCfgDirs []string, codexHomes []string)
	onHerdrSockets          func(scope string, sockets []string)
}

// NewLocator creates a Locator with the supplied options.
func NewLocator(opts LocatorOptions) (*Locator, error) {
	home := opts.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
	}
	uid := uint32(os.Geteuid())
	if opts.UID != nil {
		uid = *opts.UID
	}
	procRoot := opts.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	tmuxExec := opts.Tmux
	if tmuxExec == nil {
		tmuxExec = NewTmuxExecutor(home)
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Locator{
		home:                    home,
		uid:                     uid,
		procRoot:                procRoot,
		listPIDs:                opts.ListPIDs,
		rd:                      newCapReader(procRoot),
		tmux:                    tmuxExec,
		claudeConfigDirOverride: opts.ClaudeConfigDir,
		xdgDataHomeOverride:     opts.XDGDataHome,
		nowFn:                   nowFn,
		onDiscovered:            opts.OnDiscovered,
		onHerdrSockets:          opts.OnHerdrSockets,
	}, nil
}

// Locate executes the full or scoped agent scan corresponding to params and
// returns raw bytes in the exact NUL-delimited format of convoLocateWith.
func (l *Locator) Locate(ctx context.Context, p proto.AgentLocateParams) []byte {
	timesStr := ""
	if p.Times {
		timesStr = "times"
	}
	onlyStr := p.Only
	if onlyStr == "" && p.ShellPID > 0 {
		parts := make([]string, 0, len(p.PIDs))
		for _, pid := range p.PIDs {
			if pid > 0 {
				parts = append(parts, strconv.Itoa(pid))
			}
		}
		onlyStr = "tree:" + strconv.Itoa(p.ShellPID) + ":" + strings.Join(parts, ",")
	}
	return l.LocateWith(ctx, p.Mode, p.TmuxName, p.TmuxPane, timesStr, onlyStr)
}

// LocateWith mirrors convoLocateWith(mode, tmuxName, tmuxPane, times, only).
func (l *Locator) LocateWith(ctx context.Context, mode, tmuxName, tmuxPane, times, only string) []byte {
	var out bytes.Buffer
	var discoveredLogs []string
	var discoveredClaudeDirs []string
	var discoveredCodexHomes []string
	var discoveredHerdrSockets []string
	seenLogs := map[string]struct{}{}
	seenHerdrSockets := map[string]struct{}{}
	recordLog := func(p string) {
		if p == "" {
			return
		}
		if _, dup := seenLogs[p]; !dup {
			seenLogs[p] = struct{}{}
			discoveredLogs = append(discoveredLogs, p)
		}
	}
	recordHerdrSocket := func(s string) {
		if s == "" || strings.ContainsRune(s, '\x01') {
			return
		}
		if _, dup := seenHerdrSockets[s]; !dup {
			seenHerdrSockets[s] = struct{}{}
			discoveredHerdrSockets = append(discoveredHerdrSockets, s)
		}
	}

	// 1. Tmux pane info (only when mode == "tmux").
	if mode == "tmux" {
		t := ""
		if raw, err := l.tmux.QuerySessionPane(ctx, tmuxName); err == nil {
			t = trimTmuxOutput(raw, 800)
		}
		out.WriteString("tmux=")
		out.WriteString(t)
		out.WriteByte(0)
		if tmuxPane != "" {
			pt := ""
			if raw, err := l.tmux.QueryPaneTTY(ctx, tmuxPane); err == nil {
				pt = trimTmuxOutput(raw, 200)
			}
			out.WriteString("panetty=")
			out.WriteString(pt)
			out.WriteByte(0)
		}
	}

	// 2. /proc availability check.
	if !l.rd.isReadableFile(proto.ReadProc, filepath.Join(l.procRoot, "self", "stat")) {
		out.WriteString("noproc\x00")
		return out.Bytes()
	}

	// 3. Host clock, boot time, CLK_TCK.
	now := l.nowFn()
	out.WriteString("hostnow=")
	out.WriteString(strconv.FormatInt(now.Unix(), 10))
	out.WriteByte(0)

	btime := l.readBtime()
	hz := l.readClkTck()

	emitRec := func(pid, start, fg, procStart, sid, logPath, agentKind, via string) {
		env := l.readRecEnv(pid)
		for _, line := range strings.Split(env, "\n") {
			if val, ok := strings.CutPrefix(line, "HERDR_SOCKET_PATH="); ok && val != "" {
				recordHerdrSocket(val)
			}
		}
		tty := l.linkOf(filepath.Join(l.procRoot, pid, "fd", "0"), 200)
		anc := l.buildAncestors(pid)
		cwd := l.linkOf(filepath.Join(l.procRoot, pid, "cwd"), 1024)

		lw := ""
		lt := ""
		if logPath != "" {
			recordLog(logPath)
			logCap := agentLogCap(agentKind)
			if info, err := l.rd.stat(logCap, logPath); err == nil {
				lw = strconv.FormatInt(info.ModTime().Unix(), 10)
			}
			if times == "times" && strings.HasSuffix(logPath, ".jsonl") {
				lt = l.readLogTS(logCap, logPath, l.nowFn())
			}
		}

		ep, auth, ac := "", "", ""
		if times != "times" {
			switch agentKind {
			case "claude":
				ep, auth, ac = l.claudeEndpoint(pid, cwd)
			case "codex":
				ep, auth = l.codexEndpoint(pid)
			}
		}

		fmt.Fprintf(&out,
			"pid=%s\nstart=%s\nbtime=%s\nhz=%s\nfg=%s\nprocstart=%s\nsid=%s\nlog=%s\nlogmtime=%s\nlogts=%s\ntty=%s\ncwd=%s\nanc=%s\nagent=%s\nvia=%s\nep=%s\nauth=%s\nac=%s\n%s\x00",
			pid, start, btime, hz, fg, procStart, sid, logPath, lw, lt, tty, cwd, anc, agentKind, via, ep, auth, ac, env,
		)
	}

	seenClaude := map[string]bool{}
	emitClaude := func(pid, sessionFile, cfgDir string) bool {
		if seenClaude[pid] {
			return true
		}
		st, sid, ps, status := l.parseClaudeSession(pid, sessionFile)
		if status == 0 {
			return false
		}
		if status == 2 {
			seenClaude[pid] = true
			return true
		}
		discoveredClaudeDirs = append(discoveredClaudeDirs, cfgDir)
		logPath := l.resolveClaudeLog(pid, cfgDir, sid)
		seenClaude[pid] = true
		emitRec(pid, st.start, st.fg, ps, sid, logPath, "claude", "")
		return true
	}

	var codexes, opencodes, agys, devins []string

	if strings.HasPrefix(only, "tree:") {
		// Fast path: single herdr pane subtree verification.
		l.locateTree(only, emitClaude, &agys, &out)
	} else {
		// Full or opencode-only scan.
		procPIDs := l.listProcPIDs()
		var allowedPIDSet map[string]struct{}
		if l.listPIDs != nil {
			allowedPIDSet = make(map[string]struct{}, len(procPIDs))
			for _, pid := range procPIDs {
				allowedPIDSet[pid] = struct{}{}
			}
		}
		pidAllowed := func(pid string) bool {
			if allowedPIDSet == nil {
				return true
			}
			_, ok := allowedPIDSet[pid]
			return ok
		}

		if only != "opencode" {
			homeClaude := l.home + "/.claude"
			for _, f := range l.listClaudeSessionFiles(homeClaude) {
				if !l.rd.isRegularFile(proto.ReadAgentLogsClaude, f) {
					continue
				}
				base := filepath.Base(f)
				pid := strings.TrimSuffix(base, ".json")
				if !isAllDigits(pid) || !pidAllowed(pid) {
					continue
				}
				pidDir := filepath.Join(l.procRoot, pid)
				if !l.rd.isReadableFile(proto.ReadProc, filepath.Join(pidDir, "environ")) {
					continue
				}
				clcfg := l.claudeConfigDir(pidDir)
				cand := clcfg + "/sessions/" + pid + ".json"
				if l.rd.isRegularFile(proto.ReadAgentLogsClaude, cand) {
					emitClaude(pid, cand, clcfg)
				} else {
					emitClaude(pid, f, homeClaude)
				}
			}
			if envCfg := l.selfClaudeConfigDir(); envCfg != "" && l.rd.isDir(proto.ReadAgentLogsClaude, envCfg+"/sessions") {
				for _, f := range l.listClaudeSessionFiles(envCfg) {
					if !l.rd.isRegularFile(proto.ReadAgentLogsClaude, f) {
						continue
					}
					base := filepath.Base(f)
					pid := strings.TrimSuffix(base, ".json")
					if !isAllDigits(pid) || !pidAllowed(pid) {
						continue
					}
					pidDir := filepath.Join(l.procRoot, pid)
					if !l.rd.isReadableFile(proto.ReadProc, filepath.Join(pidDir, "environ")) {
						continue
					}
					clcfg := l.claudeConfigDir(pidDir)
					cand := clcfg + "/sessions/" + pid + ".json"
					if l.rd.isRegularFile(proto.ReadAgentLogsClaude, cand) {
						emitClaude(pid, cand, clcfg)
					}
				}
			}
		}

		// Single pass over /proc/[0-9]* owned by current uid.
		var claudes []string
		for _, pid := range procPIDs {
			pidDir := filepath.Join(l.procRoot, pid)
			if !l.rd.ownedByUID(proto.ReadProc, pidDir, l.uid) {
				continue
			}
			comm, ok := l.readCommRaw(pid)
			if !ok {
				continue
			}
			switch {
			case comm == "codex":
				codexes = append(codexes, pid)
			case comm == "opencode":
				opencodes = append(opencodes, pid)
			case comm == "agy":
				agys = append(agys, pid)
			case comm == "devin":
				devins = append(devins, pid)
			case comm == "claude":
				if only != "opencode" {
					claudes = append(claudes, pid)
				}
			case comm == "node" || comm == "bun":
				if only != "opencode" {
					if raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(pidDir, "cmdline")); err == nil && bytes.Contains(raw, []byte("claude")) {
						claudes = append(claudes, pid)
					}
				}
			case isVersionComm(comm):
				if only != "opencode" {
					if exe, err := l.rd.readLink(proto.ReadProc, filepath.Join(pidDir, "exe")); err == nil && strings.Contains(exe, "/claude/") {
						claudes = append(claudes, pid)
					}
				}
			}
		}

		for _, pid := range claudes {
			if seenClaude[pid] {
				continue
			}
			pidDir := filepath.Join(l.procRoot, pid)
			if !l.rd.isReadableFile(proto.ReadProc, filepath.Join(pidDir, "environ")) {
				continue
			}
			clcfg := l.claudeConfigDir(pidDir)
			cand := clcfg + "/sessions/" + pid + ".json"
			if l.rd.isRegularFile(proto.ReadAgentLogsClaude, cand) {
				emitClaude(pid, cand, clcfg)
			}
		}

		if only == "opencode" {
			codexes = nil
			agys = nil
			devins = nil
		}
	}

	// 4. Codex app-server candidates (skipped for tree:*).
	if !strings.HasPrefix(only, "tree:") {
		for _, pid := range codexes {
			if l.codexSub(pid) != "app-server" {
				continue
			}
			pidDir := filepath.Join(l.procRoot, pid)
			cxhome := l.codexHome(pidDir)
			discoveredCodexHomes = append(discoveredCodexHomes, cxhome)
			for _, link := range l.listFDLinks(pidDir) {
				if link == "" || strings.ContainsRune(link, '\n') {
					continue
				}
				if !matchesCodexSessionRollout(link, cxhome) {
					continue
				}
				if l.codexIsSubagent(link) {
					continue
				}
				recordLog(link)
				mtime := ""
				if info, err := l.rd.stat(proto.ReadAgentLogsCodex, link); err == nil {
					mtime = strconv.FormatInt(info.ModTime().Unix(), 10)
				}
				fmt.Fprintf(&out, "cand=%s\ncandcwd=%s\ncandts=%s\ncandmtime=%s\ncandfork=%s\x00",
					link,
					l.metaOf(link, "cwd"),
					l.metaOf(link, "timestamp"),
					mtime,
					l.metaOf(link, "forked_from_id"),
				)
			}
		}
	}

	// 5. Codex interactive processes.
	for _, pid := range codexes {
		sub := l.codexSub(pid)
		if codexNonInteractiveSubs[sub] {
			continue
		}
		if sub == "resume" {
			fmt.Fprintf(&out, "resumeof=%s %s\x00", pid, l.codexResumeOf(pid))
		}
		pidDir := filepath.Join(l.procRoot, pid)
		discoveredCodexHomes = append(discoveredCodexHomes, l.codexHome(pidDir))
		fds := l.codexFds(pidDir, l.nowFn().Unix())
		logPath := fds.cxlog
		sid := ""
		if fds.cxpend != "" {
			if sub != "" && sub != "resume" && sub != "fork" {
				continue
			}
			logPath = ""
			sid = fds.cxpend
		} else if logPath != "" {
			if sub == "" {
				if head, err := l.rd.readFileLimit(proto.ReadAgentLogsCodex, logPath, 4096); err == nil && codexExecOrigRe.Match(head) {
					continue
				}
			}
			sid = codexLogSID(logPath)
			if sid == "" {
				continue
			}
		} else {
			if sub != "" && sub != "resume" && sub != "fork" {
				continue
			}
			if fds.cxany {
				continue
			}
			sid = zeroSessionID
		}
		st, ok := l.readProcStat(pid)
		if !ok {
			continue
		}
		emitRec(pid, st.start, st.fg, st.start, sid, logPath, "codex", "")
	}

	// 6. OpenCode processes (including OpenCode 2 serve + TCP loopback matching).
	if len(opencodes) > 0 {
		var svdb []ocServerInfo
		inodeOwner := map[string]string{}
		for _, pid := range opencodes {
			pidDir := filepath.Join(l.procRoot, pid)
			if l.ocSub(pid) == "serve" {
				db := l.ocDB(pidDir)
				if db != "" && !strings.ContainsRune(db, '\t') {
					pp := l.ppidOf(pid)
					if isAllDigits(pp) {
						svdb = append(svdb, ocServerInfo{pid: pid, ppid: pp, db: db})
					}
				}
			}
			for _, link := range l.listFDLinks(pidDir) {
				if rest, ok := strings.CutPrefix(link, "socket:["); ok && strings.HasSuffix(rest, "]") {
					inode := strings.TrimSuffix(rest, "]")
					inodeOwner[inode] = pid
				}
			}
		}
		var ocports []ocPortEntry
		if len(svdb) > 0 {
			ocports = l.readOCPorts(inodeOwner)
		}
		for _, pid := range opencodes {
			if ocNonInteractiveSubs[l.ocSub(pid)] {
				continue
			}
			pidDir := filepath.Join(l.procRoot, pid)
			logPath := l.ocDB(pidDir)
			via := ""
			if logPath == "" && len(svdb) > 0 {
				via = "service"
				logPath = matchOCServiceDB(pid, ocports, svdb)
			}
			if logPath == "" && len(svdb) == 0 {
				continue
			}
			st, ok := l.readProcStat(pid)
			if !ok {
				continue
			}
			emitRec(pid, st.start, st.fg, st.start, zeroSessionID, logPath, "opencode", via)
		}
	}

	// 7. Antigravity CLI (agy) processes.
	for _, pid := range agys {
		if l.agyNonInter(pid) {
			continue
		}
		pidDir := filepath.Join(l.procRoot, pid)
		sid, logPath := l.agyFds(pidDir)
		if sid == "" {
			sid = zeroSessionID
		}
		st, ok := l.readProcStat(pid)
		if !ok {
			continue
		}
		emitRec(pid, st.start, st.fg, st.start, sid, logPath, "agy", "")
	}

	// 8. Devin CLI processes.
	for _, pid := range devins {
		if !l.dvInter(pid) {
			continue
		}
		sid := ""
		logPath := ""
		for _, kpid := range devins {
			if kpid == pid {
				continue
			}
			if l.ppidOf(kpid) != pid {
				continue
			}
			dvsid, dvdb := l.dvFds(filepath.Join(l.procRoot, kpid))
			if dvsid == "" {
				continue
			}
			sid = dvsid
			logPath = dvdb
		}
		if sid == "" {
			sid = zeroSessionID
			xdg := l.xdgDataHome()
			if xdg == "" {
				xdg = l.home + "/.local/share"
			}
			f := xdg + "/devin/cli/sessions.db"
			if !strings.ContainsRune(f, '\n') && l.rd.isRegularFile(proto.ReadAgentLogsDevin, f) {
				logPath = f
			}
		}
		st, ok := l.readProcStat(pid)
		if !ok {
			continue
		}
		emitRec(pid, st.start, st.fg, st.start, sid, logPath, "devin", "")
	}

	if l.onDiscovered != nil || l.onHerdrSockets != nil {
		if scope, ok := locateScopeForOnly(only); ok {
			if l.onDiscovered != nil {
				l.onDiscovered(scope, discoveredLogs, discoveredClaudeDirs, discoveredCodexHomes)
			}
			if l.onHerdrSockets != nil {
				l.onHerdrSockets(scope, discoveredHerdrSockets)
			}
		}
	}
	return out.Bytes()
}

func locateScopeForOnly(only string) (string, bool) {
	switch {
	case only == "":
		return LocateScopeFull, true
	case only == "opencode":
		return LocateScopeOpenCode, true
	case strings.HasPrefix(only, "tree:"):
		rest := strings.TrimPrefix(only, "tree:")
		shellPID, _, _ := strings.Cut(rest, ":")
		if isAllDigits(shellPID) {
			return "tree:" + shellPID, true
		}
		return "", false
	default:
		return "", false
	}
}

// listClaudeSessionFiles lists <cfgDir>/sessions/*.json using literal
// directory reads so glob metacharacters ([, *, ?) in cfgDir are never
// interpreted as patterns.
func (l *Locator) listClaudeSessionFiles(cfgDir string) []string {
	sessDir := cfgDir + "/sessions"
	names, err := l.rd.readDirNames(proto.ReadAgentLogsClaude, sessDir)
	if err != nil {
		return nil
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		out = append(out, sessDir+"/"+name)
	}
	return out
}

// locateTree implements the `tree:<shell>:<fg1,fg2,...>` fast path from
// convoLocateWith lines 413-524 without spawning ps or awk.
func (l *Locator) locateTree(
	only string,
	emitClaude func(pid, sessionFile, cfgDir string) bool,
	agys *[]string,
	out *bytes.Buffer,
) {
	rest := strings.TrimPrefix(only, "tree:")
	shellPID, fgRaw, hasColon := strings.Cut(rest, ":")
	if !hasColon {
		fgRaw = ""
	}
	fastInc := false
	if !isAllDigits(shellPID) {
		fastInc = true
	}
	if !fastInc {
		shellDir := filepath.Join(l.procRoot, shellPID)
		if !l.rd.ownedByUID(proto.ReadProc, shellDir, l.uid) {
			fastInc = true
		} else {
			pp := l.ppidOf(shellPID)
			if !isAllDigits(pp) || pp == "0" || pp == "1" {
				fastInc = true
			} else {
				raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, pp, "comm"))
				if err != nil || strings.ReplaceAll(string(raw), "\n", "") != "herdr" {
					fastInc = true
				}
			}
		}
	}

	var tree []string
	treeSet := map[string]bool{}
	if !fastInc {
		// Build ppid -> children map from /proc/[0-9]*/stat directly.
		kids := map[string][]string{}
		seenRoot := false
		for _, pid := range l.listProcPIDs() {
			if pid == shellPID {
				seenRoot = true
			}
			pp := l.ppidOf(pid)
			if isAllDigits(pp) {
				kids[pp] = append(kids[pp], pid)
			}
		}
		if !seenRoot {
			fastInc = true
		} else {
			q := []string{shellPID}
			treeSet[shellPID] = true
			for head := 0; head < len(q); head++ {
				cur := q[head]
				for _, child := range kids[cur] {
					if !treeSet[child] {
						treeSet[child] = true
						q = append(q, child)
						if len(q) > 64 {
							fastInc = true
							break
						}
					}
				}
				if fastInc {
					break
				}
			}
			if !fastInc {
				tree = q
			}
		}
	}

	if !fastInc && fgRaw != "" {
		for _, fp := range strings.Split(fgRaw, ",") {
			if !isAllDigits(fp) || !treeSet[fp] {
				fastInc = true
				break
			}
		}
	}

	candN := 0
	oneKind := ""
	onePID := ""
	oneFile := ""
	oneCfg := ""

	if !fastInc {
		for _, pid := range tree {
			if !isAllDigits(pid) {
				continue
			}
			pidDir := filepath.Join(l.procRoot, pid)
			if !l.rd.ownedByUID(proto.ReadProc, pidDir, l.uid) {
				fastInc = true
				break
			}
			clcfg := l.claudeConfigDir(pidDir)
			cf := clcfg + "/sessions/" + pid + ".json"
			_, _, _, clOK := l.parseClaudeSession(pid, cf)
			if clOK == 1 {
				candN++
				if candN == 1 {
					oneKind = "claude"
					onePID = pid
					oneFile = cf
					oneCfg = clcfg
				} else {
					fastInc = true
					break
				}
				continue
			} else if clOK == 2 {
				continue
			}

			comm, _ := l.readCommRaw(pid)
			switch comm {
			case "agy":
				if !l.agyNonInter(pid) {
					candN++
					if candN == 1 {
						oneKind = "agy"
						onePID = pid
					} else {
						fastInc = true
					}
				}
			case "codex", "opencode", "devin":
				fastInc = true
			}
			if fastInc {
				break
			}
		}
	}

	if !fastInc && candN == 1 {
		if oneKind == "claude" {
			if !emitClaude(onePID, oneFile, oneCfg) {
				out.WriteString("fast_incomplete=1\x00")
			}
		} else {
			*agys = append(*agys, onePID)
		}
	} else {
		out.WriteString("fast_incomplete=1\x00")
	}
}

func trimTmuxOutput(raw []byte, maxBytes int) string {
	if len(raw) > maxBytes {
		raw = raw[:maxBytes]
	}
	return strings.ReplaceAll(string(raw), "\n", "")
}
