//go:build linux

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	// DefaultMaxInotifyWatches caps the number of individual log files watched
	// via Linux inotify before automatically degrading to 1-second stat polling.
	DefaultMaxInotifyWatches = 256

	// DefaultWatchPollInterval is the cadence for checking /proc PID additions,
	// exits, exec transitions, and stat polling in poll-fallback mode.
	DefaultWatchPollInterval = 1 * time.Second

	// maxExtraLogsPerSub caps explicit per-subscriber log paths.
	maxExtraLogsPerSub = 1024
)

// WatchMode describes the active file monitoring mode.
const (
	WatchModeIdle    = "idle"
	WatchModeInotify = "inotify"
	WatchModePoll    = "poll"
)

// WatchEmitter receives hint-only agent.changed events produced by Watcher.
type WatchEmitter func(eventName string, data any) (proto.Event, error)

// WatchWatcherOptions configures a Watcher instance.
type WatchWatcherOptions struct {
	Locator      *Locator
	Validator    *PathValidator
	Emit         WatchEmitter
	MaxWatches   int
	PollInterval time.Duration
}

type watchSub struct {
	logs map[string]struct{}
}

// procEntry records identity, executable metadata, agent classification, and
// active log signature of an owned process so Watcher detects (pid, starttime)
// reuse, in-place exec transitions (including agent-to-agent exec), and log
// set changes on an existing agent PID.
type procEntry struct {
	start       string
	comm        string
	exe         string
	cmdlineHash [32]byte
	agentKind   string
	logSig      string
}

func (e procEntry) isAgent() bool {
	return e.agentKind != ""
}

// Watcher monitors agent log file growth (via Linux inotify, with automatic
// 1-second stat polling fallback when the watched file count exceeds
// MaxWatches or inotify returns an error) and agent process set changes.
type Watcher struct {
	loc          *Locator
	val          *PathValidator
	emit         WatchEmitter
	maxWatches   int
	pollInterval time.Duration
	rd           *capReader

	// lifecycleMu strictly serializes Subscribe/Unsubscribe/Stop transitions
	// so a new run() loop can never start before a stopping run() loop has
	// completely exited and closed its inotify FD. Lock order: lifecycleMu -> mu.
	lifecycleMu sync.Mutex

	mu             sync.Mutex
	subs           map[any]*watchSub
	mode           string // WatchModeIdle, WatchModeInotify, WatchModePoll
	fallbackReason string // "", "limit", or "inotify_error"
	watchedCount   int
	running        bool
	cancel         context.CancelFunc
	doneCh         chan struct{}
	syncCh         chan struct{}

	// Loop-managed inotify & file state (map access synchronized by mu).
	inotifyFD   int
	inotifyFile *os.File
	wdToPath    map[int32]string
	pathToWD    map[string]int32
	fileSizes   map[string]int64

	// Process tracking state (accessed only by the single active run goroutine).
	ownedProcs map[int]procEntry
}

// NewWatcher constructs a paused Watcher. It consumes zero goroutines and zero
// inotify descriptors until the first subscriber attaches via Subscribe.
func NewWatcher(opts WatchWatcherOptions) *Watcher {
	maxW := opts.MaxWatches
	if maxW <= 0 {
		maxW = DefaultMaxInotifyWatches
	}
	interval := opts.PollInterval
	if interval <= 0 {
		interval = DefaultWatchPollInterval
	}
	procRoot := "/proc"
	if opts.Locator != nil && opts.Locator.procRoot != "" {
		procRoot = opts.Locator.procRoot
	}
	return &Watcher{
		loc:          opts.Locator,
		val:          opts.Validator,
		emit:         opts.Emit,
		maxWatches:   maxW,
		pollInterval: interval,
		rd:           newCapReader(procRoot),
		subs:         make(map[any]*watchSub),
		mode:         WatchModeIdle,
		syncCh:       make(chan struct{}, 1),
		inotifyFD:    -1,
		wdToPath:     make(map[int32]string),
		pathToWD:     make(map[string]int32),
		fileSizes:    make(map[string]int64),
		ownedProcs:   make(map[int]procEntry),
	}
}

// WatchStatus summarizes the current watcher state for agent.watch responses
// and daemon.health reporting.
type WatchStatus struct {
	Watching       bool
	Mode           string
	FallbackReason string
	WatchedFiles   int
	MaxFiles       int
}

// Status returns the current watcher status.
func (w *Watcher) Status() WatchStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WatchStatus{
		Watching:       w.running,
		Mode:           w.mode,
		FallbackReason: w.fallbackReason,
		WatchedFiles:   w.watchedCount,
		MaxFiles:       w.maxWatches,
	}
}

// Health returns the capability health string for daemon.health's caps["watch"].
func (w *Watcher) Health() string {
	st := w.Status()
	if st.Mode == WatchModePoll {
		if st.FallbackReason == "limit" {
			return fmt.Sprintf("degraded:poll_fallback(limit=%d)", st.MaxFiles)
		}
		return "degraded:poll_fallback(inotify_error)"
	}
	return "ok"
}

// NotifyDiscovered triggers a watch-set refresh when a locate scan discovers
// new log files.
func (w *Watcher) NotifyDiscovered() {
	select {
	case w.syncCh <- struct{}{}:
	default:
	}
}

// Subscribe registers or updates sub as an active watcher and sets its
// validated extra log paths (cleaned up automatically when sub unsubscribes).
// Serialized by lifecycleMu so a new run loop never overlaps a stopping one.
func (w *Watcher) Subscribe(sub any, logs []string) WatchStatus {
	validSet := make(map[string]struct{}, len(logs))
	for _, p := range logs {
		if len(validSet) >= maxExtraLogsPerSub {
			break
		}
		if _, ok := w.val.ValidatePath(p); ok {
			validSet[p] = struct{}{}
		}
	}

	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()

	w.mu.Lock()
	entry, exists := w.subs[sub]
	if !exists {
		entry = &watchSub{logs: validSet}
		w.subs[sub] = entry
	} else {
		// Replace this subscriber's explicit log list so repeated watch calls
		// do not accumulate stale paths indefinitely.
		entry.logs = validSet
	}

	needStart := !w.running
	if needStart {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		w.cancel = cancel
		w.doneCh = done
		w.running = true
		w.wdToPath = make(map[int32]string)
		w.pathToWD = make(map[string]int32)
		w.fileSizes = make(map[string]int64)
		w.ownedProcs = make(map[int]procEntry)

		targets := w.collectTargetLogsLocked()
		w.watchedCount = len(targets)
		if len(targets) > w.maxWatches {
			w.mode = WatchModePoll
			w.fallbackReason = "limit"
		} else {
			w.mode = WatchModeInotify
			w.fallbackReason = ""
		}
		go w.run(ctx, done)
	}
	w.mu.Unlock()

	if !needStart {
		w.NotifyDiscovered()
	}
	return w.Status()
}

// Unsubscribe detaches sub and removes its extraLogs. When the last subscriber
// leaves, Unsubscribe waits for the watcher loop to exit completely before
// releasing lifecycleMu so a concurrent Subscribe cannot launch a second loop.
func (w *Watcher) Unsubscribe(sub any) WatchStatus {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()

	w.mu.Lock()
	delete(w.subs, sub)
	var cancel context.CancelFunc
	var done chan struct{}
	stopping := len(w.subs) == 0 && w.running
	if stopping {
		cancel = w.cancel
		done = w.doneCh
	}
	w.mu.Unlock()

	if stopping {
		cancel()
		<-done
		w.mu.Lock()
		w.running = false
		w.cancel = nil
		w.doneCh = nil
		w.mode = WatchModeIdle
		w.fallbackReason = ""
		w.watchedCount = 0
		w.mu.Unlock()
	} else {
		w.NotifyDiscovered()
	}
	return w.Status()
}

// Stop shuts down the watcher immediately regardless of active subscribers,
// waiting until the background loop and inotify reader have exited.
func (w *Watcher) Stop() {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()

	w.mu.Lock()
	w.subs = make(map[any]*watchSub)
	cancel := w.cancel
	done := w.doneCh
	running := w.running
	w.mu.Unlock()

	if running && cancel != nil {
		cancel()
		<-done
	}

	w.mu.Lock()
	w.running = false
	w.cancel = nil
	w.doneCh = nil
	w.mode = WatchModeIdle
	w.fallbackReason = ""
	w.watchedCount = 0
	w.mu.Unlock()
}

func (w *Watcher) collectTargetLogsLocked() []string {
	seen := make(map[string]struct{}, 16)
	for _, sub := range w.subs {
		for p := range sub.logs {
			seen[p] = struct{}{}
		}
	}
	for _, p := range w.val.KnownLogs() {
		seen[p] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (w *Watcher) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	w.seedProcesses()
	w.reconcileWatches()

	inotifyCh := make(chan []string, 16)
	var readerWG sync.WaitGroup
	var readerCancel context.CancelFunc

	startReader := func() {
		if w.inotifyFile == nil || readerCancel != nil {
			return
		}
		rctx, rcancel := context.WithCancel(ctx)
		readerCancel = rcancel
		f := w.inotifyFile
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			w.readInotifyLoop(rctx, f, inotifyCh)
		}()
	}
	stopReader := func() {
		if readerCancel != nil {
			readerCancel()
			if w.inotifyFile != nil {
				_ = w.inotifyFile.SetReadDeadline(time.Now())
			}
			readerWG.Wait()
			readerCancel = nil
		}
	}
	defer func() {
		stopReader()
		w.closeInotify()
	}()

	startReader()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	syncWatches := func() {
		wasFile := w.inotifyFile
		w.reconcileWatches()
		if wasFile != nil && w.inotifyFile == nil {
			stopReader()
		} else if wasFile == nil && w.inotifyFile != nil {
			startReader()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.syncCh:
			syncWatches()
		case changed := <-inotifyCh:
			if len(changed) > 0 {
				w.emitChanged(nil, nil, changed)
			}
		case <-ticker.C:
			added, removed := w.pollProcesses()
			var changedLogs []string
			if w.Status().Mode == WatchModePoll {
				changedLogs = w.pollFileGrowth()
			}
			// Reconcile if processes changed OR if any target log was rotated/
			// unlinked and is currently missing an active inotify watch.
			if len(added) > 0 || len(removed) > 0 || w.needsWatchRebind() {
				syncWatches()
			}
			if len(added) > 0 || len(removed) > 0 || len(changedLogs) > 0 {
				w.emitChanged(added, removed, changedLogs)
			}
		}
	}
}

func (w *Watcher) needsWatchRebind() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mode != WatchModeInotify {
		return false
	}
	targets := w.collectTargetLogsLocked()
	if len(targets) != len(w.pathToWD) {
		return true
	}
	for _, p := range targets {
		if _, ok := w.pathToWD[p]; !ok {
			return true
		}
	}
	return false
}

func (w *Watcher) emitChanged(added, removed []int, logs []string) {
	if w.emit == nil {
		return
	}
	sort.Ints(added)
	sort.Ints(removed)
	sort.Strings(logs)
	_, _ = w.emit(proto.EventAgentChanged, proto.AgentChangedEventData{
		AddedPIDs:   added,
		RemovedPIDs: removed,
		ChangedLogs: logs,
	})
}

func (w *Watcher) openInotify() error {
	if w.inotifyFD >= 0 {
		return nil
	}
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return err
	}
	w.inotifyFD = fd
	w.inotifyFile = os.NewFile(uintptr(fd), "corrald-inotify")
	return nil
}

func (w *Watcher) closeInotify() {
	if w.inotifyFile != nil {
		_ = w.inotifyFile.Close()
		w.inotifyFile = nil
		w.inotifyFD = -1
	} else if w.inotifyFD >= 0 {
		_ = syscall.Close(w.inotifyFD)
		w.inotifyFD = -1
	}
	w.mu.Lock()
	w.wdToPath = make(map[int32]string)
	w.pathToWD = make(map[string]int32)
	w.mu.Unlock()
}

func (w *Watcher) reconcileWatches() {
	w.mu.Lock()
	targets := w.collectTargetLogsLocked()
	w.mu.Unlock()

	validTargets := make([]string, 0, len(targets))
	initialSizes := make(map[string]int64, len(targets))
	for _, p := range targets {
		f, _, ok := w.val.OpenChecked(p)
		if !ok {
			continue
		}
		if info, err := f.Stat(); err == nil {
			initialSizes[p] = info.Size()
			validTargets = append(validTargets, p)
		}
		_ = f.Close()
	}

	targetSet := make(map[string]struct{}, len(validTargets))
	for _, p := range validTargets {
		targetSet[p] = struct{}{}
	}

	w.mu.Lock()
	for p, sz := range initialSizes {
		if _, exists := w.fileSizes[p]; !exists {
			w.fileSizes[p] = sz
		}
	}
	for p := range w.fileSizes {
		if _, ok := targetSet[p]; !ok {
			delete(w.fileSizes, p)
		}
	}
	w.mu.Unlock()

	if len(validTargets) > w.maxWatches {
		w.closeInotify()
		w.mu.Lock()
		w.mode = WatchModePoll
		w.fallbackReason = "limit"
		w.watchedCount = len(validTargets)
		w.mu.Unlock()
		return
	}

	if err := w.openInotify(); err != nil {
		w.closeInotify()
		w.mu.Lock()
		w.mode = WatchModePoll
		w.fallbackReason = "inotify_error"
		w.watchedCount = len(validTargets)
		w.mu.Unlock()
		return
	}

	w.mu.Lock()
	var toRemove []struct {
		path string
		wd   int32
	}
	for p, wd := range w.pathToWD {
		if _, ok := targetSet[p]; !ok {
			toRemove = append(toRemove, struct {
				path string
				wd   int32
			}{p, wd})
			delete(w.pathToWD, p)
			delete(w.wdToPath, wd)
		}
	}
	var toAdd []string
	for _, p := range validTargets {
		if _, exists := w.pathToWD[p]; !exists {
			toAdd = append(toAdd, p)
		}
	}
	w.mu.Unlock()

	for _, rm := range toRemove {
		_, _ = syscall.InotifyRmWatch(w.inotifyFD, uint32(rm.wd))
	}

	const mask = syscall.IN_MODIFY | syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF
	for _, p := range toAdd {
		wd, err := syscall.InotifyAddWatch(w.inotifyFD, p, mask)
		if err != nil {
			if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EMFILE) {
				w.closeInotify()
				w.mu.Lock()
				w.mode = WatchModePoll
				w.fallbackReason = "inotify_error"
				w.watchedCount = len(validTargets)
				w.mu.Unlock()
				return
			}
			continue
		}
		w.mu.Lock()
		w.pathToWD[p] = int32(wd)
		w.wdToPath[int32(wd)] = p
		w.mu.Unlock()
	}

	w.mu.Lock()
	w.mode = WatchModeInotify
	w.fallbackReason = ""
	w.watchedCount = len(w.pathToWD)
	w.mu.Unlock()
}

func (w *Watcher) readInotifyLoop(ctx context.Context, f *os.File, out chan<- []string) {
	buf := make([]byte, 4096)
	rawFD := int(f.Fd())
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return
			}
			var ne interface{ Timeout() bool }
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		if n < syscall.SizeofInotifyEvent {
			continue
		}
		changedSet := make(map[string]struct{})
		off := 0
		for off+syscall.SizeofInotifyEvent <= n {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			wd := ev.Wd
			mask := ev.Mask
			nameLen := int(binary.LittleEndian.Uint32(buf[off+12 : off+16]))
			off += syscall.SizeofInotifyEvent + nameLen

			w.mu.Lock()
			path := w.wdToPath[wd]
			w.mu.Unlock()
			if path == "" {
				continue
			}
			if mask&(syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF|syscall.IN_IGNORED) != 0 {
				// Evict stale watch mapping immediately, emit a change hint
				// right away so writes to a rotated/recreated file before
				// reconcileWatches completes are never missed, and schedule a
				// watch re-bind.
				w.mu.Lock()
				delete(w.wdToPath, wd)
				if w.pathToWD[path] == wd {
					delete(w.pathToWD, path)
				}
				delete(w.fileSizes, path)
				w.mu.Unlock()
				if mask&syscall.IN_IGNORED == 0 {
					_, _ = syscall.InotifyRmWatch(rawFD, uint32(wd))
				}
				changedSet[path] = struct{}{}
				w.NotifyDiscovered()
				continue
			}
			if mask&syscall.IN_MODIFY != 0 {
				if w.checkFileGrew(path) {
					changedSet[path] = struct{}{}
				}
			}
		}
		if len(changedSet) > 0 {
			list := make([]string, 0, len(changedSet))
			for p := range changedSet {
				list = append(list, p)
			}
			select {
			case out <- list:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (w *Watcher) checkFileGrew(path string) bool {
	capCat := inferLogCap(path)
	info, err := w.rd.stat(capCat, path)
	if err != nil {
		return false
	}
	sz := info.Size()
	w.mu.Lock()
	prev, had := w.fileSizes[path]
	w.fileSizes[path] = sz
	w.mu.Unlock()
	return !had || sz > prev
}

func (w *Watcher) pollFileGrowth() []string {
	w.mu.Lock()
	paths := make([]string, 0, len(w.fileSizes))
	for p := range w.fileSizes {
		paths = append(paths, p)
	}
	w.mu.Unlock()

	var changed []string
	for _, p := range paths {
		if w.checkFileGrew(p) {
			changed = append(changed, p)
		}
	}
	return changed
}

func (w *Watcher) listPIDNames() ([]string, error) {
	if w.loc != nil && w.loc.listPIDs != nil {
		return w.loc.listPIDs()
	}
	return w.rd.readDirNames(proto.ReadProc, w.loc.procRoot)
}

// seedProcesses builds the initial owned-process table keyed by PID with
// (starttime, comm, exe, cmdlineHash, agentKind, logSig).
func (w *Watcher) seedProcesses() {
	names, err := w.listPIDNames()
	if err != nil {
		return
	}
	w.ownedProcs = make(map[int]procEntry, 64)
	for _, name := range names {
		if !isAllDigits(name) {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		if ent, ok := w.inspectOwnedPID(name); ok {
			w.ownedProcs[pid] = ent
		}
	}
}

// pollProcesses compares the current owned process set by (pid, starttime),
// re-classifies existing PIDs whose comm, exe, or cmdline changed (including
// agent-to-agent exec), and detects active log set changes on known agents.
func (w *Watcher) pollProcesses() (added, removed []int) {
	names, err := w.listPIDNames()
	if err != nil {
		return nil, nil
	}

	curPIDs := make(map[int]struct{}, len(names))
	nextOwned := make(map[int]procEntry, len(w.ownedProcs))

	for _, name := range names {
		if !isAllDigits(name) {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		curPIDs[pid] = struct{}{}

		prev, hadPrev := w.ownedProcs[pid]
		if !hadPrev {
			ent, ok := w.inspectOwnedPID(name)
			if !ok {
				continue
			}
			nextOwned[pid] = ent
			if ent.isAgent() {
				added = append(added, pid)
			}
			continue
		}

		// Existing owned PID: check starttime, comm, exe, and (for node/bun or
		// known agents) cmdline to catch PID reuse and in-place exec().
		pidDir := filepath.Join(w.loc.procRoot, name)
		st, ok := w.loc.readProcStat(name)
		if !ok {
			if prev.isAgent() {
				removed = append(removed, pid)
			}
			continue
		}
		comm, ok := w.loc.readCommRaw(name)
		if !ok {
			if prev.isAgent() {
				removed = append(removed, pid)
			}
			continue
		}
		exe, _ := w.rd.readLink(proto.ReadProc, filepath.Join(pidDir, "exe"))

		startChanged := st.start != prev.start
		execChanged := comm != prev.comm || exe != prev.exe
		needCmdCheck := comm == "node" || comm == "bun" || prev.comm == "node" || prev.comm == "bun" || prev.isAgent()
		if needCmdCheck && !startChanged && !execChanged {
			if w.readCmdlineHash(pidDir) != prev.cmdlineHash {
				execChanged = true
			}
		}

		if !startChanged && !execChanged {
			if prev.isAgent() {
				logSig := w.agentLogSignature(name, pidDir, prev.agentKind)
				if logSig != prev.logSig {
					prev.logSig = logSig
					added = append(added, pid)
				}
			}
			nextOwned[pid] = prev
			continue
		}

		// Re-classify on PID reuse or exec transition.
		ent, ok := w.inspectOwnedPID(name)
		if !ok {
			if prev.isAgent() {
				removed = append(removed, pid)
			}
			continue
		}
		nextOwned[pid] = ent
		if startChanged {
			if prev.isAgent() {
				removed = append(removed, pid)
			}
			if ent.isAgent() {
				added = append(added, pid)
			}
		} else {
			if prev.isAgent() && !ent.isAgent() {
				removed = append(removed, pid)
			} else if !prev.isAgent() && ent.isAgent() {
				added = append(added, pid)
			} else if prev.isAgent() && ent.isAgent() && (prev.agentKind != ent.agentKind || prev.logSig != ent.logSig) {
				removed = append(removed, pid)
				added = append(added, pid)
			}
		}
	}

	for pid, prev := range w.ownedProcs {
		if _, alive := curPIDs[pid]; !alive && prev.isAgent() {
			removed = append(removed, pid)
		}
	}

	w.ownedProcs = nextOwned
	return added, removed
}

func (w *Watcher) readCmdlineHash(pidDir string) [32]byte {
	raw, err := w.rd.readFile(proto.ReadProc, filepath.Join(pidDir, "cmdline"))
	if err != nil || len(raw) == 0 {
		return [32]byte{}
	}
	return sha256.Sum256(raw)
}

func (w *Watcher) agentLogSignature(pid, pidDir, kind string) string {
	switch kind {
	case "claude":
		clcfg := w.loc.claudeConfigDir(pidDir)
		cf := clcfg + "/sessions/" + pid + ".json"
		if _, sid, _, status := w.loc.parseClaudeSession(pid, cf); status == 1 {
			return sid + "\x00" + w.loc.resolveClaudeLog(pid, clcfg, sid)
		}
		return ""
	case "codex":
		if w.loc.codexSub(pid) == "app-server" {
			cxhome := w.loc.codexHome(pidDir)
			var rollouts []string
			for _, link := range w.loc.listFDLinks(pidDir) {
				if link != "" && !strings.ContainsRune(link, '\n') && matchesCodexSessionRollout(link, cxhome) {
					rollouts = append(rollouts, link)
				}
			}
			sort.Strings(rollouts)
			return strings.Join(rollouts, "\x00")
		}
		fds := w.loc.codexFds(pidDir, w.loc.nowFn().Unix())
		if fds.cxlog == "" && fds.cxpend == "" {
			return ""
		}
		return fds.cxlog + "\x00" + fds.cxpend
	case "opencode":
		return w.loc.ocDB(pidDir)
	case "agy":
		sid, logPath := w.loc.agyFds(pidDir)
		if sid == "" && logPath == "" {
			return ""
		}
		return sid + "\x00" + logPath
	case "devin":
		sid, db := w.loc.dvFds(pidDir)
		if sid == "" && db == "" {
			return ""
		}
		return sid + "\x00" + db
	default:
		return ""
	}
}

func (w *Watcher) inspectOwnedPID(pid string) (procEntry, bool) {
	pidDir := filepath.Join(w.loc.procRoot, pid)
	if !w.rd.ownedByUID(proto.ReadProc, pidDir, w.loc.uid) {
		return procEntry{}, false
	}
	st, ok := w.loc.readProcStat(pid)
	if !ok {
		return procEntry{}, false
	}
	comm, ok := w.loc.readCommRaw(pid)
	if !ok {
		return procEntry{}, false
	}
	exe, _ := w.rd.readLink(proto.ReadProc, filepath.Join(pidDir, "exe"))

	var cmdHash [32]byte
	var cmdHashed bool
	agentKind := ""
	switch {
	case comm == "codex" || comm == "opencode" || comm == "agy" || comm == "devin" || comm == "claude":
		agentKind = comm
	case comm == "node" || comm == "bun":
		if raw, err := w.rd.readFile(proto.ReadProc, filepath.Join(pidDir, "cmdline")); err == nil {
			cmdHash = sha256.Sum256(raw)
			cmdHashed = true
			if bytes.Contains(raw, []byte("claude")) {
				agentKind = "claude"
			}
		}
	case isVersionComm(comm):
		if strings.Contains(exe, "/claude/") {
			agentKind = "claude"
		}
	default:
		clcfg := w.loc.claudeConfigDir(pidDir)
		cf := clcfg + "/sessions/" + pid + ".json"
		if _, _, _, status := w.loc.parseClaudeSession(pid, cf); status == 1 {
			agentKind = "claude"
		}
	}

	logSig := ""
	if agentKind != "" {
		if !cmdHashed {
			cmdHash = w.readCmdlineHash(pidDir)
		}
		logSig = w.agentLogSignature(pid, pidDir, agentKind)
	}

	return procEntry{
		start:       st.start,
		comm:        comm,
		exe:         exe,
		cmdlineHash: cmdHash,
		agentKind:   agentKind,
		logSig:      logSig,
	}, true
}
