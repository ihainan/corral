//go:build linux

package agent

import (
	"context"
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

	// DefaultWatchPollInterval is the cadence for checking /proc PID additions
	// and exits, as well as stat polling when in poll-fallback mode.
	DefaultWatchPollInterval = 1 * time.Second
)

// WatchMode describes the active file monitoring mode.
const (
	WatchModeIdle    = "idle"
	WatchModeInotify = "inotify"
	WatchModePoll    = "poll"
)

// WatchEmitter receives hint-only agent.changed events produced by Watcher.
type WatchEmitter func(eventName string, data any) (proto.Event, error)

// WatcherOptions configures a Watcher instance.
type WatchWatcherOptions struct {
	Locator      *Locator
	Validator    *PathValidator
	Emit         WatchEmitter
	MaxWatches   int
	PollInterval time.Duration
}

// Watcher monitors agent log file growth (via Linux inotify, with automatic
// 1-second stat polling fallback when the watched file count exceeds
// MaxWatches or inotify returns an error) and agent process set changes (via
// lightweight 1-second /proc directory-name diffing).
type Watcher struct {
	loc          *Locator
	val          *PathValidator
	emit         WatchEmitter
	maxWatches   int
	pollInterval time.Duration
	rd           *capReader

	mu             sync.Mutex
	subscribers    map[any]struct{}
	extraLogs      map[string]struct{} // validated logs explicitly requested by agent.watch
	mode           string              // WatchModeIdle, WatchModeInotify, WatchModePoll
	fallbackReason string              // "", "limit", or "inotify_error"
	watchedCount   int
	running        bool
	cancel         context.CancelFunc
	doneCh         chan struct{}
	syncCh         chan struct{}

	// Internal loop state (accessed only by run goroutine).
	inotifyFD   int
	inotifyFile *os.File
	wdToPath    map[int32]string
	pathToWD    map[string]int32
	fileSizes   map[string]int64
	allPIDs     map[int]struct{}
	agentPIDs   map[int]string // pid -> starttime
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
		subscribers:  make(map[any]struct{}),
		extraLogs:    make(map[string]struct{}),
		mode:         WatchModeIdle,
		syncCh:       make(chan struct{}, 1),
		inotifyFD:    -1,
		wdToPath:     make(map[int32]string),
		pathToWD:     make(map[string]int32),
		fileSizes:    make(map[string]int64),
		allPIDs:      make(map[int]struct{}),
		agentPIDs:    make(map[int]string),
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

// Subscribe registers sub as an active watcher and optionally adds validated
// log paths to the watch set. Starts the background watch loop on the first
// subscriber.
func (w *Watcher) Subscribe(sub any, logs []string) WatchStatus {
	var validLogs []string
	for _, p := range logs {
		if _, ok := w.val.ValidatePath(p); ok {
			validLogs = append(validLogs, p)
		}
	}

	w.mu.Lock()
	w.subscribers[sub] = struct{}{}
	for _, p := range validLogs {
		w.extraLogs[p] = struct{}{}
	}
	needStart := !w.running
	if needStart {
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		w.doneCh = make(chan struct{})
		w.running = true
		// Seed initial mode synchronously so the response reflects current state.
		targets := w.collectTargetLogsLocked()
		w.watchedCount = len(targets)
		if len(targets) > w.maxWatches {
			w.mode = WatchModePoll
			w.fallbackReason = "limit"
		} else {
			w.mode = WatchModeInotify
			w.fallbackReason = ""
		}
		go w.run(ctx, w.doneCh)
	}
	w.mu.Unlock()

	if !needStart {
		w.NotifyDiscovered()
	}
	// Give the newly started loop a brief moment to reconcile initial watches if
	// it just started, or simply return current status.
	return w.Status()
}

// Unsubscribe detaches sub. When the last subscriber leaves, the watcher loop
// stops and releases all inotify descriptors.
func (w *Watcher) Unsubscribe(sub any) WatchStatus {
	w.mu.Lock()
	delete(w.subscribers, sub)
	var cancel context.CancelFunc
	var done chan struct{}
	if len(w.subscribers) == 0 && w.running {
		cancel = w.cancel
		done = w.doneCh
		w.running = false
		w.cancel = nil
		w.doneCh = nil
		w.mode = WatchModeIdle
		w.fallbackReason = ""
		w.watchedCount = 0
	}
	w.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
	return w.Status()
}

// Stop shuts down the watcher immediately regardless of active subscribers.
func (w *Watcher) Stop() {
	w.mu.Lock()
	cancel := w.cancel
	done := w.doneCh
	w.subscribers = make(map[any]struct{})
	w.running = false
	w.cancel = nil
	w.doneCh = nil
	w.mode = WatchModeIdle
	w.fallbackReason = ""
	w.watchedCount = 0
	w.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
}

func (w *Watcher) collectTargetLogsLocked() []string {
	seen := make(map[string]struct{}, len(w.extraLogs)+16)
	for p := range w.extraLogs {
		seen[p] = struct{}{}
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
	defer w.closeInotify()

	// Seed baseline PID set and agent PIDs without emitting spurious events.
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
	defer stopReader()

	startReader()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.syncCh:
			wasInotify := w.inotifyFile != nil
			w.reconcileWatches()
			if wasInotify && w.inotifyFile == nil {
				stopReader()
			} else if !wasInotify && w.inotifyFile != nil {
				startReader()
			}
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
			if len(added) > 0 || len(removed) > 0 {
				wasInotify := w.inotifyFile != nil
				w.reconcileWatches()
				if wasInotify && w.inotifyFile == nil {
					stopReader()
				} else if !wasInotify && w.inotifyFile != nil {
					startReader()
				}
			}
			if len(added) > 0 || len(removed) > 0 || len(changedLogs) > 0 {
				w.emitChanged(added, removed, changedLogs)
			}
		}
	}
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

	// Update fileSizes under lock (preserving existing sizes, pruning removed files).
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

	// If over limit, degrade to 1s poll mode and release inotify watches.
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

	// Remove stale watches.
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

	// Add missing watches.
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

// seedProcesses builds the initial PID set and agent PID map.
func (w *Watcher) seedProcesses() {
	names, err := w.rd.readDirNames(proto.ReadProc, w.loc.procRoot)
	if err != nil {
		return
	}
	w.allPIDs = make(map[int]struct{}, len(names))
	w.agentPIDs = make(map[int]string)
	for _, name := range names {
		if !isAllDigits(name) {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		w.allPIDs[pid] = struct{}{}
		if st, ok := w.classifyAgentPID(name); ok {
			w.agentPIDs[pid] = st
		}
	}
}

// pollProcesses performs a lightweight /proc directory-name scan, inspecting
// only newly appeared PIDs and checking whether previously tracked agent PIDs
// exited. When idle (no new PIDs and no exited agents), it performs only a
// single /proc Readdirnames call and zero per-PID file opens.
func (w *Watcher) pollProcesses() (added, removed []int) {
	names, err := w.rd.readDirNames(proto.ReadProc, w.loc.procRoot)
	if err != nil {
		return nil, nil
	}
	curAll := make(map[int]struct{}, len(names))
	for _, name := range names {
		if !isAllDigits(name) {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		curAll[pid] = struct{}{}
		if _, seen := w.allPIDs[pid]; !seen {
			if st, ok := w.classifyAgentPID(name); ok {
				w.agentPIDs[pid] = st
				added = append(added, pid)
			}
		}
	}
	for pid := range w.agentPIDs {
		if _, alive := curAll[pid]; !alive {
			delete(w.agentPIDs, pid)
			removed = append(removed, pid)
		}
	}
	w.allPIDs = curAll
	return added, removed
}

// classifyAgentPID reports whether pid belongs to the current UID and looks
// like a tracked agent process (claude/codex/opencode/agy/devin).
func (w *Watcher) classifyAgentPID(pid string) (string, bool) {
	pidDir := filepath.Join(w.loc.procRoot, pid)
	if !w.rd.ownedByUID(proto.ReadProc, pidDir, w.loc.uid) {
		return "", false
	}
	comm, ok := w.loc.readCommRaw(pid)
	if !ok {
		return "", false
	}
	isAgent := false
	switch {
	case comm == "codex" || comm == "opencode" || comm == "agy" || comm == "devin" || comm == "claude":
		isAgent = true
	case comm == "node" || comm == "bun":
		if raw, err := w.rd.readFile(proto.ReadProc, filepath.Join(pidDir, "cmdline")); err == nil && strings.Contains(string(raw), "claude") {
			isAgent = true
		}
	case isVersionComm(comm):
		if exe, err := w.rd.readLink(proto.ReadProc, filepath.Join(pidDir, "exe")); err == nil && strings.Contains(exe, "/claude/") {
			isAgent = true
		}
	default:
		// Also recognize fake/renamed processes that have a valid Claude session file.
		clcfg := w.loc.claudeConfigDir(pidDir)
		cf := clcfg + "/sessions/" + pid + ".json"
		if _, _, _, status := w.loc.parseClaudeSession(pid, cf); status == 1 {
			isAgent = true
		}
	}
	if !isAgent {
		return "", false
	}
	st, ok := w.loc.readProcStat(pid)
	if !ok {
		return "", false
	}
	return st.start, true
}
