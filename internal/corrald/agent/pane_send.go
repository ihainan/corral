//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	defaultPaneLockTimeout = 10 * time.Second

	// Timings preserved verbatim from convoGated (Claude Code, Codex, agy, and
	// OpenCode are sensitive to paste/key/screen pacing).
	gatedPastePause    = 300 * time.Millisecond
	gatedKeyPause      = 300 * time.Millisecond
	gatedPalettePause  = 300 * time.Millisecond
	gatedScreenPause   = 700 * time.Millisecond
	gatedPollStep      = 100 * time.Millisecond
	gatedPastedIters   = 47
	gatedTakenWaitIter = 30
	gatedTakenChkIter  = 10
)

var (
	claudeSessionIDRe  = regexp.MustCompile(`"sessionId" *: *"([0-9a-fA-F-]*)"`)
	claudeStatusRe     = regexp.MustCompile(`"status" *: *"([a-z]*)"`)
	claudeWaitingForRe = regexp.MustCompile(`"waitingFor" *: *"([a-z ]*)"`)
)

// PaneSender executes gated pane typing, key presses, and prompt/card answers
// directly on the host with identical semantics, pacing, exit codes (0-19),
// and stdout bytes to Corral's convoGated shell script.
type PaneSender struct {
	locator     *Locator
	val         *PathValidator
	tmux        *TmuxExecutor
	herdr       *HerdrExecutor
	sleepFn     func(ctx context.Context, d time.Duration) error
	lockTimeout time.Duration

	bufSeq atomic.Uint64

	lockMu    sync.Mutex
	paneLocks map[string]*paneLockSlot
}

type paneLockSlot struct {
	ch chan struct{}
	n  int
}

// NewPaneSender constructs a PaneSender using loc, val, tmux, and herdr.
func NewPaneSender(loc *Locator, val *PathValidator, tmux *TmuxExecutor, herdr *HerdrExecutor) *PaneSender {
	return &PaneSender{
		locator:     loc,
		val:         val,
		tmux:        tmux,
		herdr:       herdr,
		sleepFn:     contextSleep,
		lockTimeout: defaultPaneLockTimeout,
		paneLocks:   make(map[string]*paneLockSlot),
	}
}

// SetLockTimeoutForTest overrides the per-pane lock timeout for fast unit tests.
func (ps *PaneSender) SetLockTimeoutForTest(d time.Duration) {
	ps.lockTimeout = d
}

func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (ps *PaneSender) acquirePaneLock(ctx context.Context, key string) (release func(), ok bool) {
	timeout := ps.lockTimeout
	if timeout <= 0 {
		timeout = defaultPaneLockTimeout
	}
	ps.lockMu.Lock()
	slot := ps.paneLocks[key]
	if slot == nil {
		slot = &paneLockSlot{ch: make(chan struct{}, 1)}
		ps.paneLocks[key] = slot
	}
	slot.n++
	ps.lockMu.Unlock()

	leave := func() {
		ps.lockMu.Lock()
		slot.n--
		if slot.n == 0 {
			delete(ps.paneLocks, key)
		}
		ps.lockMu.Unlock()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case slot.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-slot.ch
				leave()
			})
		}, true
	case <-timer.C:
		leave()
		return nil, false
	case <-ctx.Done():
		leave()
		return nil, false
	}
}

// ActionCapForGate returns the declared action capability constant for a gate:
// answer-card for prompt/card replies and pane-input for all other pane writes.
func ActionCapForGate(gate string) string {
	switch {
	case strings.HasPrefix(gate, "reply:"),
		strings.HasPrefix(gate, "creply:"),
		strings.HasPrefix(gate, "preply:"),
		strings.HasPrefix(gate, "areply:"),
		strings.HasPrefix(gate, "card:"):
		return proto.ActionAnswerCard
	default:
		return proto.ActionPaneInput
	}
}

type parsedLenSpec struct {
	isKeys   bool
	wantLen  int
	parts    []int
	tabsend  bool
	altmsend bool
	noenter  bool
	haspic   bool
}

func parseLenSpec(raw string) (parsedLenSpec, bool) {
	var spec parsedLenSpec
	if raw == "keys" {
		spec.isKeys = true
		return spec, true
	}
	s := raw
	if strings.HasSuffix(s, "~") {
		spec.tabsend = true
		s = strings.TrimSuffix(s, "~")
		if s == "" || !allCharsIn(s, isDigitOrPlus) {
			return spec, false
		}
	}
	if strings.HasSuffix(s, "!") {
		spec.altmsend = true
		s = strings.TrimSuffix(s, "!")
		if !isAllDigits(s) {
			return spec, false
		}
	}
	if strings.ContainsRune(s, '+') {
		if !allCharsIn(s, isDigitOrPlus) || strings.HasPrefix(s, "+") || strings.HasSuffix(s, "+") || strings.Contains(s, "++") {
			return spec, false
		}
		segs := strings.Split(s, "+")
		parts := make([]int, 0, len(segs))
		total := 0
		for _, x := range segs {
			// Matches shell `case $x in 0*|????????*) exit 4;; esac`:
			// none empty, none starting with 0, none >= 8 digits (<= 9999999).
			if x == "" || x[0] == '0' || len(x) >= 8 {
				return spec, false
			}
			v, err := strconv.Atoi(x)
			if err != nil {
				return spec, false
			}
			parts = append(parts, v)
			total += v
		}
		spec.parts = parts
		spec.wantLen = total
		s = strconv.Itoa(total)
	}
	if strings.HasSuffix(s, "-") {
		spec.noenter = true
		s = strings.TrimSuffix(s, "-")
		if !isAllDigits(s) {
			return spec, false
		}
	}
	if strings.HasSuffix(s, "@") {
		spec.haspic = true
		s = strings.TrimSuffix(s, "@")
		if !isAllDigits(s) {
			return spec, false
		}
	}
	if !isAllDigits(s) {
		return spec, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return spec, false
	}
	spec.wantLen = v
	return spec, true
}

func isDigitOrPlus(b byte) bool {
	return (b >= '0' && b <= '9') || b == '+'
}

func allCharsIn(s string, fn func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !fn(s[i]) {
			return false
		}
	}
	return true
}

type parsedGate struct {
	gate   string
	cx     bool
	cxplan bool
	oc     string
	agy    string
}

func parseGate(raw string) parsedGate {
	g := parsedGate{gate: raw}
	if g.gate == "cfree" || strings.HasPrefix(g.gate, "creply:") || strings.HasPrefix(g.gate, "cscreen:") {
		g.cx = true
		g.gate = g.gate[1:]
	}
	if strings.HasPrefix(g.gate, "preply:") {
		g.cx = true
		g.cxplan = true
		g.gate = g.gate[1:]
	}
	if strings.HasPrefix(g.gate, "ofree:") {
		g.oc = strings.TrimPrefix(g.gate, "ofree:")
		g.gate = "free"
	}
	switch {
	case strings.HasPrefix(g.gate, "afree:"):
		g.agy = strings.TrimPrefix(g.gate, "afree:")
		g.gate = "free"
	case g.gate == "amode":
		g.agy = "present"
		g.gate = "free"
	case strings.HasPrefix(g.gate, "areply:"):
		g.agy = "reply"
		g.gate = "reply:" + strings.TrimPrefix(g.gate, "areply:")
	}
	return g
}

// gatedRun holds per-invocation mutable state mirroring convoGated's shell
// variables so helper methods (front, waiting, owner, owned, gateEnter,
// boxBefore, pasted, taken, readFinalScreen) mirror the script cleanly.
type gatedRun struct {
	ps        *PaneSender
	ctx       context.Context
	p         proto.PaneSendParams
	pg        parsedGate
	ls        parsedLenSpec
	rd        int
	actionCap string
	pidStr    string
	startStr  string
	cmd1      byte
	partBytes [][]byte

	clcfg string
	kind  string

	// Last owner() result.
	osStr string
	orOK  bool
	oVal  string

	// Last waiting() result.
	st string
	wf string

	// Claude Code box tracking (boxbefore / pasted / taken).
	cc bool
	b0 string
	b1 string

	typing bool
}

// Send executes a single gated pane send/key/answer operation.
func (ps *PaneSender) Send(ctx context.Context, p proto.PaneSendParams) proto.PaneSendResult {
	pg := parseGate(p.Gate)
	rd := p.Read
	if rd < 0 {
		rd = 0
	} else if rd > 60 {
		rd = 60
	}
	ls, ok := parseLenSpec(p.LenSpec)
	if !ok {
		return proto.PaneSendResult{Exit: 4}
	}

	// Acquire per-pane host lock (10s timeout -> exit 7).
	lockKey := p.Mode + "\x00" + p.Pane
	release, locked := ps.acquirePaneLock(ctx, lockKey)
	if !locked {
		if ctx.Err() != nil {
			return proto.PaneSendResult{Exit: 4}
		}
		return proto.PaneSendResult{Exit: 7}
	}
	defer release()

	r := &gatedRun{
		ps:        ps,
		ctx:       ctx,
		p:         p,
		pg:        pg,
		ls:        ls,
		rd:        rd,
		actionCap: ActionCapForGate(p.Gate),
		pidStr:    strconv.FormatInt(p.PID, 10),
		startStr:  strconv.FormatInt(p.Start, 10),
	}

	if !ls.isKeys {
		if len(p.Text) != ls.wantLen {
			return proto.PaneSendResult{Exit: 4}
		}
		if len(p.Text) > 0 && p.Text[0] != '\n' {
			r.cmd1 = p.Text[0]
		}
		if len(ls.parts) > 0 {
			r.partBytes = make([][]byte, 0, len(ls.parts))
			at := 0
			for _, x := range ls.parts {
				if at+x > len(p.Text) {
					return proto.PaneSendResult{Exit: 4}
				}
				r.partBytes = append(r.partBytes, p.Text[at:at+x])
				at += x
			}
		}
	}

	// If herdr mode and socket hasn't been cached by an earlier agent.locate yet,
	// run one host locate scan so legitimately running herdr sockets are known
	// while still strictly rejecting any socket not observed in locate results.
	if p.Mode == "herdr" && p.Socket != "" && ps.val != nil && !ps.val.HasHerdrSocket(p.Socket) {
		_ = ps.locator.LocateWith(ctx, "", "", "", "", "")
	}

	if !r.front() {
		return proto.PaneSendResult{Exit: 3}
	}

	if pg.gate != "" {
		if code := r.checkInitialGate(); code != 0 {
			return proto.PaneSendResult{Exit: code}
		}
	}

	if p.Mode == "tmux" {
		return r.runTmux()
	}
	return r.runHerdr()
}

func (r *gatedRun) sleep(d time.Duration) bool {
	return r.ps.sleepFn(r.ctx, d) == nil
}

func (r *gatedRun) readPlainScreen(agyRecent bool) ([]byte, bool) {
	if r.p.Mode == "tmux" {
		b, err := r.ps.tmux.CapturePane(r.ctx, r.p.Pane, false)
		if err != nil {
			return nil, false
		}
		return b, true
	}
	if agyRecent {
		if txt, err := r.ps.herdr.ReadPane(r.ctx, r.p.Socket, r.p.Pane, "recent", 60, false); err == nil {
			return []byte(txt), true
		}
	}
	txt, err := r.ps.herdr.ReadPane(r.ctx, r.p.Socket, r.p.Pane, "visible", 0, false)
	if err != nil {
		return nil, false
	}
	return []byte(txt), true
}

func (r *gatedRun) readANSIScreen(agyRecent bool) ([]byte, bool) {
	if r.p.Mode == "tmux" {
		b, err := r.ps.tmux.CapturePane(r.ctx, r.p.Pane, true)
		if err != nil {
			return nil, false
		}
		return b, true
	}
	if agyRecent {
		if txt, err := r.ps.herdr.ReadPane(r.ctx, r.p.Socket, r.p.Pane, "recent", 60, true); err == nil {
			return []byte(txt), true
		}
	}
	txt, err := r.ps.herdr.ReadPane(r.ctx, r.p.Socket, r.p.Pane, "visible", 0, true)
	if err != nil {
		return nil, false
	}
	return []byte(txt), true
}

func (r *gatedRun) readFinalScreen() (out []byte, exitCode int) {
	if r.rd <= 0 {
		return nil, 0
	}
	if !r.sleep(gatedScreenPause) {
		if r.typing {
			return nil, 6
		}
		return nil, 4
	}
	raw, ok := r.readPlainScreen(r.kind == "agy")
	if !ok {
		return nil, 8
	}
	return tailNonBlankLines(string(raw), r.rd), 0
}

func (r *gatedRun) owner(after bool) {
	r.osStr = ""
	r.orOK = false
	r.oVal = "none"
	raw, ok := r.readPlainScreen(false)
	if !ok {
		return
	}
	r.orOK = true
	r.osStr = strings.TrimRight(string(raw), "\n")
	r.oVal = claudeOwner(r.osStr, after)
}

func (r *gatedRun) owned(after, typed bool) int {
	if r.kind != "claude" || r.pg.cx {
		return 0
	}
	switch {
	case r.pg.gate == "free" || r.pg.gate == "enter" || r.pg.gate == "idle" || r.pg.gate == "stop" || strings.HasPrefix(r.pg.gate, "box:"):
	default:
		return 0
	}
	if typed && !r.front() {
		return 16
	}
	r.owner(after)
	if r.oVal == "main" {
		return 0
	}
	if r.pg.gate == "stop" && r.oVal == "none" {
		if !r.orOK {
			return 19
		}
		if r.osStr == "" {
			return 15
		}
		if claudeHasOtherMainLine(r.osStr) {
			return 13
		}
		return 0
	}
	if typed {
		return 16
	}
	if !r.orOK {
		return 19
	}
	switch r.oVal {
	case "subagent":
		return 13
	case "list", "sessions":
		return 14
	default:
		return 15
	}
}

func (r *gatedRun) front() bool {
	if r.p.PID <= 0 || r.p.Start <= 0 {
		return false
	}
	loc := r.ps.locator
	st, ok := loc.readProcStat(r.pidStr)
	if !ok || st.start != r.startStr || st.fg != "1" {
		return false
	}
	pidDir := filepath.Join(loc.procRoot, r.pidStr)
	paneEnvCap := proto.ReadEnvHerdrPaneID
	paneEnvKey := "HERDR_PANE_ID"
	if r.p.Mode == "tmux" {
		paneEnvCap = proto.ReadEnvTmuxPane
		paneEnvKey = "TMUX_PANE"
	}
	kvs, err := loc.rd.readProcEnv(pidDir, paneEnvCap, proto.ReadEnvClaudeConfigDir)
	if err != nil {
		return false
	}
	paneMatch := false
	for _, kv := range kvs {
		if kv.key == paneEnvKey && kv.val == r.p.Pane {
			paneMatch = true
			break
		}
	}
	if !paneMatch {
		return false
	}
	if r.clcfg == "" {
		r.clcfg = loc.claudeConfigDir(pidDir)
	}

	comm, _ := loc.readCommRaw(r.pidStr)
	if len(comm) > 16 {
		comm = comm[:16]
	}
	r.kind = "claude"
	switch comm {
	case "codex":
		r.kind = "codex"
		cs := loc.codexSub(r.pidStr)
		if codexNonInteractiveSubs[cs] {
			return false
		}
		fds := loc.codexFds(pidDir, loc.nowFn().Unix())
		if fds.cxnew {
			return false
		}
		if fds.cxpend != "" {
			if cs != "" && cs != "resume" && cs != "fork" {
				return false
			}
			if r.p.SID != fds.cxpend && !(r.p.SID == zeroSessionID && !fds.cxany) {
				return false
			}
		} else if r.p.SID == zeroSessionID {
			if cs != "" || fds.cxany {
				return false
			}
		} else if fds.cxlog == "" {
			if cs != "" && cs != "resume" && cs != "fork" {
				return false
			}
			ownh := loc.codexHome(pidDir)
			if !r.codexAppServerHoldsSID(ownh, r.p.SID) {
				return false
			}
		} else {
			if codexLogSID(fds.cxlog) != r.p.SID {
				return false
			}
			if cs == "" {
				if head, err := loc.rd.readFileLimit(proto.ReadAgentLogsCodex, fds.cxlog, 4096); err == nil && codexExecOrigRe.Match(head) {
					return false
				}
			}
		}
	case "opencode":
		r.kind = "opencode"
		if ocNonInteractiveSubs[loc.ocSub(r.pidStr)] {
			return false
		}
	case "agy":
		r.kind = "agy"
		if loc.agyNonInter(r.pidStr) {
			return false
		}
		agysid, _ := loc.agyFds(pidDir)
		if r.p.SID == zeroSessionID {
			if agysid != "" {
				return false
			}
		} else if agysid != r.p.SID {
			return false
		}
	default:
		sessPath := r.clcfg + "/sessions/" + r.pidStr + ".json"
		raw, err := loc.rd.readFileLimit(proto.ReadAgentLogsClaude, sessPath, 65536)
		if err != nil {
			return false
		}
		c := strings.ReplaceAll(string(raw), "\n", "")
		m := claudeSessionIDRe.FindStringSubmatch(c)
		if m == nil || m[1] != r.p.SID {
			return false
		}
	}

	if r.p.Mode == "tmux" {
		rawTTY, err := r.ps.tmux.QueryPaneTTY(r.ctx, r.p.Pane)
		if err != nil {
			return false
		}
		tty := strings.TrimRight(string(rawTTY), "\n")
		if tty == "" {
			return false
		}
		fd0, err := loc.rd.readLink(proto.ReadProc, filepath.Join(pidDir, "fd", "0"))
		if err != nil || fd0 != tty {
			return false
		}
	}
	return true
}

func (r *gatedRun) codexAppServerHoldsSID(ownh, sid string) bool {
	loc := r.ps.locator
	for _, pid := range loc.listProcPIDs() {
		pidDir := filepath.Join(loc.procRoot, pid)
		if !loc.rd.ownedByUID(proto.ReadProc, pidDir, loc.uid) {
			continue
		}
		comm, ok := loc.readCommRaw(pid)
		if !ok || comm != "codex" {
			continue
		}
		for _, link := range loc.listFDLinks(pidDir) {
			if matchesCodexSessionRollout(link, ownh) && codexLogSID(link) == sid {
				return true
			}
		}
	}
	return false
}

func (r *gatedRun) waiting() {
	if r.pg.oc != "" {
		r.st, r.wf = "title-unread", ""
		raw, ok := r.readPlainScreen(false)
		if !ok {
			return
		}
		ocs := strings.TrimRight(string(raw), "\n")
		if ocs == "" {
			return
		}
		r.st = "waiting"
		d, hasBox := ocBoxDigest(ocs)
		if !hasBox {
			return
		}
		if d == r.pg.oc {
			r.st = "idle"
		} else {
			r.st = "draft"
		}
		return
	}
	if r.pg.agy != "" {
		r.st, r.wf = "title-unread", ""
		raw, ok := r.readPlainScreen(true)
		if !ok {
			return
		}
		ags := strings.TrimRight(string(raw), "\n")
		if ags == "" {
			return
		}
		d, hasBox := agyBoxDigest(ags)
		if r.pg.agy == "reply" {
			if !hasBox {
				r.st, r.wf = "waiting", "permission prompt"
			} else {
				r.st = "idle"
			}
			return
		}
		r.st = "waiting"
		if !hasBox {
			return
		}
		if r.pg.agy == "present" {
			r.st = "idle"
			return
		}
		if d == r.pg.agy {
			r.st = "idle"
		} else {
			r.st = "draft"
		}
		return
	}
	if r.pg.cxplan {
		r.st, r.wf = "waiting", "plan"
		return
	}
	if r.pg.cx {
		r.st, r.wf = "title-unread", ""
		var title string
		if r.p.Mode == "tmux" {
			raw, err := r.ps.tmux.QueryPaneTitle(r.ctx, r.p.Pane)
			if err != nil {
				return
			}
			title = strings.TrimRight(string(raw), "\n")
		} else {
			t, has, err := r.ps.herdr.GetPaneTitle(r.ctx, r.p.Socket, r.p.Pane)
			if err != nil || !has {
				return
			}
			title = t
		}
		if strings.HasPrefix(title, "[ ! ] Action Required") || strings.HasPrefix(title, "[ . ] Action Required") {
			r.st, r.wf = "waiting", "permission prompt"
		} else {
			r.st = "busy"
		}
		return
	}

	loc := r.ps.locator
	if r.clcfg == "" {
		r.clcfg = loc.claudeConfigDir(filepath.Join(loc.procRoot, r.pidStr))
	}
	raw, _ := loc.rd.readFileLimit(proto.ReadAgentLogsClaude, r.clcfg+"/sessions/"+r.pidStr+".json", 65536)
	c := strings.ReplaceAll(string(raw), "\n", "")
	r.st, r.wf = "", ""
	if m := claudeStatusRe.FindStringSubmatch(c); m != nil {
		r.st = m[1]
	}
	if m := claudeWaitingForRe.FindStringSubmatch(c); m != nil {
		r.wf = m[1]
	}
}

func (r *gatedRun) checkInitialGate() int {
	r.waiting()
	g := r.pg.gate
	switch {
	case g == "free" || g == "enter":
		if r.st == "waiting" {
			return 9
		}
		if r.st == "title-unread" {
			return 11
		}
		if r.st == "draft" {
			return 17
		}
	case g == "stop":
		// Checked by owned() below.
	case strings.HasPrefix(g, "box:"):
		if r.st == "waiting" {
			return 9
		}
		raw, ok := r.readPlainScreen(false)
		if !ok {
			return 10
		}
		bs := strings.TrimRight(string(raw), "\n")
		if claudeBoxDigest(bs) != strings.TrimPrefix(g, "box:") {
			return 10
		}
	case strings.HasPrefix(g, "screen:"):
		if r.st == "waiting" {
			return 9
		}
		if r.st == "title-unread" {
			return 11
		}
		raw, _ := r.readPlainScreen(false)
		if steadyDigest(raw) != strings.TrimPrefix(g, "screen:") {
			return 10
		}
	case g == "idle":
		if r.st != "idle" {
			return 12
		}
	case g == "dialog" || g == "panel" || strings.HasPrefix(g, "panel:"):
		if r.st != "waiting" {
			return 9
		}
		switch r.wf {
		case "permission prompt", "input needed", "sandbox request", "worker request":
			return 9
		}
		r.owner(false)
		if r.osStr == "" || r.oVal != "none" {
			return 9
		}
		if strings.HasPrefix(g, "panel:") {
			raw, _ := r.readANSIScreen(false)
			if panelDigest(raw) != strings.TrimPrefix(g, "panel:") {
				return 10
			}
		}
	case g == "look":
		// Any state.
	case strings.HasPrefix(g, "look:"):
		raw, ok := r.readANSIScreen(r.kind == "agy")
		if !ok || len(raw) == 0 {
			return 10
		}
		if lookDigest(raw) != strings.TrimPrefix(g, "look:") {
			return 10
		}
	case strings.HasPrefix(g, "reply:"):
		if r.st != "waiting" {
			return 9
		}
		switch r.wf {
		case "permission prompt", "input needed", "sandbox request", "worker request", "plan":
		default:
			return 9
		}
		raw, _ := r.readPlainScreen(false)
		if steadyDigest(raw) != strings.TrimPrefix(g, "reply:") {
			return 10
		}
	case strings.HasPrefix(g, "card:"):
		if r.st == "waiting" {
			return 9
		}
		raw, ok := r.readANSIScreen(false)
		if !ok {
			raw = []byte("\n\x01corral: unread\n")
		}
		if cardRegionDigest(raw) != strings.TrimPrefix(g, "card:") {
			return 10
		}
	default:
		return 4
	}

	if r.ls.isKeys && (g == "enter" || strings.HasPrefix(g, "box:")) {
		return r.owned(true, false)
	}
	return r.owned(false, false)
}

func (r *gatedRun) boxRead() (string, bool) {
	raw, ok := r.readPlainScreen(false)
	if !ok {
		return "", false
	}
	bs := strings.TrimRight(string(raw), "\n")
	txt, _ := claudeBoxText(bs)
	return txt, true
}

func (r *gatedRun) boxBefore() {
	if r.kind != "claude" || r.pg.cx || r.pg.oc != "" || r.ls.noenter {
		return
	}
	switch r.pg.gate {
	case "", "free", "enter", "idle":
	default:
		return
	}
	r.owner(false)
	if r.oVal != "main" {
		return
	}
	r.b0, _ = claudeBoxText(r.osStr)
	r.cc = true
	if r.b0 != "" && !r.ls.haspic {
		bs0 := claudeBoxSqueeze(r.b0)
		bsw := claudeBoxSqueeze(string(r.p.Text))
		if bs0 != "" && bs0 == bsw {
			r.b1 = r.b0
		}
	}
}

func (r *gatedRun) pasted() {
	if !r.cc || r.b1 != "" {
		return
	}
	for n := 0; n < gatedPastedIters; n++ {
		bx, ok := r.boxRead()
		if !ok {
			return
		}
		if bx != r.b0 {
			r.b1 = bx
			return
		}
		if !r.sleep(gatedPollStep) {
			return
		}
	}
}

func (r *gatedRun) gateEnter() int {
	if r.kind == "claude" && !r.pg.cx && r.pg.gate == "free" && r.cmd1 == '/' {
		if !r.front() {
			return 16
		}
		r.waiting()
		if r.st == "title-unread" || r.st == "waiting" {
			if !(r.st == "waiting" && r.wf == "dialog open") {
				return 5
			}
		}
		r.owner(true)
		if r.oVal == "none" && r.osStr != "" && claudePalette(r.osStr) {
			if r.p.Mode == "tmux" {
				if err := r.ps.tmux.SendKey(r.ctx, r.actionCap, r.p.Pane, "Escape"); err != nil {
					return 6
				}
			} else {
				if err := r.ps.herdr.SendKeys(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, []string{"esc"}); err != nil {
					return 6
				}
			}
			if !r.sleep(gatedPalettePause) {
				return 6
			}
		}
	}
	if r.pg.gate == "" {
		return 0
	}
	r.waiting()
	switch {
	case r.pg.gate == "free" || r.pg.gate == "enter" || strings.HasPrefix(r.pg.gate, "screen:"):
		if r.st == "waiting" || r.st == "title-unread" {
			return 5
		}
	case r.pg.gate == "idle":
		if r.st != "idle" {
			return 5
		}
	case strings.HasPrefix(r.pg.gate, "reply:"):
		if r.st != "waiting" {
			return 5
		}
	}
	return r.owned(true, true)
}

func (r *gatedRun) taken() int {
	if r.cc && r.b1 == "" {
		for n := 0; n < gatedTakenWaitIter; n++ {
			if !r.sleep(gatedPollStep) {
				return 6
			}
			bx, ok := r.boxRead()
			if !ok {
				return 0
			}
			if bx != r.b0 {
				r.b1 = bx
				break
			}
		}
	}
	if r.b1 == "" {
		return 0
	}
	for k := 1; k <= 2; k++ {
		for n := 0; n < gatedTakenChkIter; n++ {
			if !r.sleep(gatedPollStep) {
				return 6
			}
			bx, ok := r.boxRead()
			if !ok {
				return 0
			}
			if bx != r.b1 {
				if r.ls.haspic && claudeImageTagRe.MatchString(bx) {
					r.b1 = bx
					continue
				}
				return 0
			}
		}
		if k == 2 {
			return 18
		}
		if code := r.gateEnter(); code != 0 {
			return code
		}
		if r.p.Mode == "tmux" {
			if err := r.ps.tmux.SendKey(r.ctx, r.actionCap, r.p.Pane, "Enter"); err != nil {
				return 18
			}
		} else {
			if err := r.ps.herdr.SendKeys(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, []string{"enter"}); err != nil {
				return 18
			}
		}
	}
	return 0
}

func (r *gatedRun) nextBufName() string {
	return fmt.Sprintf("corral-send-%d-%d", os.Getpid(), r.ps.bufSeq.Add(1))
}

func (r *gatedRun) runTmux() proto.PaneSendResult {
	rawSess, err := r.ps.tmux.QueryPaneSessionName(r.ctx, r.p.Pane)
	if err != nil || strings.TrimRight(string(rawSess), "\n") != r.p.Session {
		return proto.PaneSendResult{Exit: 3}
	}

	if r.ls.isKeys {
		if !r.front() {
			return proto.PaneSendResult{Exit: 3}
		}
		if len(r.p.Keys) > 0 {
			r.typing = true
		}
		for _, k := range r.p.Keys {
			if k == "pause" {
				if !r.sleep(gatedKeyPause) {
					return proto.PaneSendResult{Exit: 6}
				}
				continue
			}
			if err := r.ps.tmux.SendKey(r.ctx, r.actionCap, r.p.Pane, k); err != nil {
				return proto.PaneSendResult{Exit: 6}
			}
		}
		out, rc := r.readFinalScreen()
		return proto.PaneSendResult{Exit: rc, Out: out}
	}

	bufName := r.nextBufName()
	bufLoaded := false
	defer func() {
		if bufLoaded {
			delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = r.ps.tmux.DeleteBuffer(delCtx, r.actionCap, bufName)
			cancel()
		}
	}()

	if len(r.partBytes) > 0 {
		if !r.front() {
			return proto.PaneSendResult{Exit: 3}
		}
		r.typing = true
		for _, part := range r.partBytes {
			if err := r.ps.tmux.LoadBuffer(r.ctx, r.actionCap, bufName, part); err != nil {
				return proto.PaneSendResult{Exit: 6}
			}
			bufLoaded = true
			if err := r.ps.tmux.PasteBuffer(r.ctx, r.actionCap, bufName, r.p.Pane); err != nil {
				return proto.PaneSendResult{Exit: 6}
			}
			bufLoaded = false
			if !r.sleep(gatedPastePause) {
				return proto.PaneSendResult{Exit: 6}
			}
		}
	} else {
		if err := r.ps.tmux.LoadBuffer(r.ctx, r.actionCap, bufName, r.p.Text); err != nil {
			return proto.PaneSendResult{Exit: 4}
		}
		bufLoaded = true
		if !r.front() {
			return proto.PaneSendResult{Exit: 3}
		}
		r.boxBefore()
		r.typing = true
		if err := r.ps.tmux.PasteBuffer(r.ctx, r.actionCap, bufName, r.p.Pane); err != nil {
			return proto.PaneSendResult{Exit: 6}
		}
		bufLoaded = false
		if !r.sleep(gatedPastePause) {
			return proto.PaneSendResult{Exit: 6}
		}
	}

	if r.ls.noenter {
		out, rc := r.readFinalScreen()
		return proto.PaneSendResult{Exit: rc, Out: out}
	}

	r.pasted()
	if code := r.gateEnter(); code != 0 {
		return proto.PaneSendResult{Exit: code}
	}

	submitKey := "Enter"
	if r.ls.tabsend {
		submitKey = "Tab"
	} else if r.ls.altmsend {
		submitKey = "M-m"
	}
	if err := r.ps.tmux.SendKey(r.ctx, r.actionCap, r.p.Pane, submitKey); err != nil {
		return proto.PaneSendResult{Exit: 5}
	}

	if code := r.taken(); code != 0 {
		return proto.PaneSendResult{Exit: code}
	}
	out, rc := r.readFinalScreen()
	return proto.PaneSendResult{Exit: rc, Out: out}
}

func (r *gatedRun) runHerdr() proto.PaneSendResult {
	if r.ls.isKeys {
		if !r.front() {
			return proto.PaneSendResult{Exit: 3}
		}
		if len(r.p.Keys) > 0 {
			r.typing = true
		}
		for _, k := range r.p.Keys {
			if k == "pause" {
				if !r.sleep(gatedKeyPause) {
					return proto.PaneSendResult{Exit: 6}
				}
			} else if k == "shift+tab" {
				if err := r.ps.herdr.SendText(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, "\x1b[Z"); err != nil {
					return proto.PaneSendResult{Exit: 6}
				}
			} else {
				if err := r.ps.herdr.SendKeys(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, []string{k}); err != nil {
					return proto.PaneSendResult{Exit: 6}
				}
			}
		}
		out, rc := r.readFinalScreen()
		return proto.PaneSendResult{Exit: rc, Out: out}
	}

	if len(r.partBytes) > 0 {
		if !r.front() {
			return proto.PaneSendResult{Exit: 3}
		}
		r.typing = true
		for _, part := range r.partBytes {
			payload := "\x1b[200~" + string(part) + "\x1b[201~"
			if err := r.ps.herdr.SendText(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, payload); err != nil {
				return proto.PaneSendResult{Exit: 6}
			}
			if !r.sleep(gatedPastePause) {
				return proto.PaneSendResult{Exit: 6}
			}
		}
	} else {
		if !r.front() {
			return proto.PaneSendResult{Exit: 3}
		}
		r.boxBefore()
		r.typing = true
		payload := "\x1b[200~" + string(r.p.Text) + "\x1b[201~"
		if err := r.ps.herdr.SendText(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, payload); err != nil {
			return proto.PaneSendResult{Exit: 6}
		}
		if !r.sleep(gatedPastePause) {
			return proto.PaneSendResult{Exit: 6}
		}
	}

	if r.ls.noenter {
		out, rc := r.readFinalScreen()
		return proto.PaneSendResult{Exit: rc, Out: out}
	}

	r.pasted()
	if code := r.gateEnter(); code != 0 {
		return proto.PaneSendResult{Exit: code}
	}

	if r.ls.altmsend {
		if err := r.ps.herdr.SendKeys(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, []string{"alt+m"}); err != nil {
			if err2 := r.ps.herdr.SendText(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, "\x1bm"); err2 != nil {
				return proto.PaneSendResult{Exit: 5}
			}
		}
	} else {
		k := "enter"
		if r.ls.tabsend {
			k = "tab"
		}
		if err := r.ps.herdr.SendKeys(r.ctx, r.actionCap, r.p.Socket, r.p.Pane, []string{k}); err != nil {
			return proto.PaneSendResult{Exit: 5}
		}
	}

	if code := r.taken(); code != 0 {
		return proto.PaneSendResult{Exit: code}
	}
	out, rc := r.readFinalScreen()
	return proto.PaneSendResult{Exit: rc, Out: out}
}
