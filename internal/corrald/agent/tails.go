//go:build linux

package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	convoTailBytes           = 16384
	convoMsgTailWindowBytes  = 4194304 // 4 MiB
	convoMsgTailOutputBytes  = 65536   // 64 KiB
	convoMsgTailMaxLineBytes = 65536

	// Upper bounds for tracked locate authorization entries so long-lived
	// daemons never accumulate unbounded state.
	maxTrackedLogs     = 4096
	maxTrackedDirs     = 256
	maxTreeAuthBuckets = 64
)

var messageTailTypeRe = regexp.MustCompile(`"type"[ ]*:[ ]*"(message|user_message|agent_message|item_completed|custom_tool_call|function_call|task_started|task_complete|thread_rolled_back)"`)

type authBucket struct {
	knownLogs     map[string]string // literal clean path string -> capability category
	claudeCfgDirs map[string]struct{}
	codexHomes    map[string]struct{}
	lastUsed      atomic.Uint64
}

func newAuthBucket(logs, claudeDirs, codexHomes []string, seq uint64) *authBucket {
	b := &authBucket{
		knownLogs:     make(map[string]string, len(logs)),
		claudeCfgDirs: make(map[string]struct{}, len(claudeDirs)),
		codexHomes:    make(map[string]struct{}, len(codexHomes)),
	}
	b.lastUsed.Store(seq)
	for _, p := range logs {
		if len(b.knownLogs) >= maxTrackedLogs {
			break
		}
		if !isCleanAbsPath(p) {
			continue
		}
		b.knownLogs[p] = inferLogCap(p)
	}
	for _, d := range claudeDirs {
		if len(b.claudeCfgDirs) >= maxTrackedDirs {
			break
		}
		if !isCleanAbsPath(d) {
			continue
		}
		b.claudeCfgDirs[d] = struct{}{}
	}
	for _, d := range codexHomes {
		if len(b.codexHomes) >= maxTrackedDirs {
			break
		}
		if !isCleanAbsPath(d) {
			continue
		}
		b.codexHomes[d] = struct{}{}
	}
	return b
}

// PathValidator restricts log file reads to regular files owned by the daemon's
// UID whose literal path string either appeared in an active scoped locate
// bucket (full, opencode, or per-shell tree:<shell>) or resides under a
// declared reads agent log root directory, opened strictly via
// openat(O_NOFOLLOW|O_DIRECTORY) across every path component so symlinks can
// neither poison authorization nor race parent directory checks.
type PathValidator struct {
	home string
	uid  uint32

	useSeq atomic.Uint64

	mu             sync.RWMutex
	fullBucket     *authBucket
	opencodeBucket *authBucket
	treeBuckets    map[string]*authBucket
	herdrBuckets   map[string]map[string]struct{}
	xdgDataHome    string
	selfClaudeDir  string
}

type allowedRoot struct {
	dir string
	cap string
}

// NewPathValidator creates a PathValidator for (home, uid).
func NewPathValidator(home string, uid uint32, _ string) *PathValidator {
	cleanHome := ""
	if home != "" {
		cleanHome = filepath.Clean(home)
	}
	return &PathValidator{
		home:         cleanHome,
		uid:          uid,
		treeBuckets:  make(map[string]*authBucket),
		herdrBuckets: make(map[string]map[string]struct{}),
	}
}

// SetEnvDirs sets the self CLAUDE_CONFIG_DIR and XDG_DATA_HOME used when
// computing declared agent log root directories.
func (v *PathValidator) SetEnvDirs(claudeCfgDir, xdgDataHome string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if claudeCfgDir != "" && filepath.IsAbs(claudeCfgDir) && !strings.ContainsAny(claudeCfgDir, "\x00\n") {
		v.selfClaudeDir = filepath.Clean(claudeCfgDir)
	} else {
		v.selfClaudeDir = ""
	}
	if xdgDataHome != "" && filepath.IsAbs(xdgDataHome) && !strings.ContainsAny(xdgDataHome, "\x00\n") {
		v.xdgDataHome = filepath.Clean(xdgDataHome)
	} else {
		v.xdgDataHome = ""
	}
}

// UpdateDiscovered replaces the authorization bucket for scope ("full",
// "opencode", or "tree:<shell>") with the literal path strings seen during
// that locate scan. It NEVER resolves symlinks or adds symlink targets to the
// authorization set. Distinct "tree:*" scopes are capped at maxTreeAuthBuckets
// with LRU eviction.
func (v *PathValidator) UpdateDiscovered(scope string, logs, claudeDirs, codexHomes []string) {
	seq := v.useSeq.Add(1)
	b := newAuthBucket(logs, claudeDirs, codexHomes, seq)

	v.mu.Lock()
	defer v.mu.Unlock()

	switch {
	case scope == "" || scope == LocateScopeFull:
		v.fullBucket = b
	case scope == LocateScopeOpenCode:
		v.opencodeBucket = b
	case strings.HasPrefix(scope, "tree:"):
		if _, exists := v.treeBuckets[scope]; !exists && len(v.treeBuckets) >= maxTreeAuthBuckets {
			var oldestKey string
			var oldestSeq uint64
			first := true
			for k, tb := range v.treeBuckets {
				u := tb.lastUsed.Load()
				if first || u < oldestSeq {
					oldestKey = k
					oldestSeq = u
					first = false
				}
			}
			if !first {
				delete(v.treeBuckets, oldestKey)
			}
		}
		v.treeBuckets[scope] = b
	}
}

// KnownLogs returns a deduplicated snapshot of currently tracked literal
// located log paths across all active locate buckets.
func (v *PathValidator) KnownLogs() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	seen := make(map[string]struct{})
	addBucket := func(b *authBucket) {
		if b == nil {
			return
		}
		for p := range b.knownLogs {
			seen[p] = struct{}{}
		}
	}
	addBucket(v.fullBucket)
	addBucket(v.opencodeBucket)
	for _, b := range v.treeBuckets {
		addBucket(b)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

// UpdateHerdrSockets records literal herdr Unix socket paths discovered during
// locate for the given scope ("full", "opencode", "tree:<shell>", or "restored").
func (v *PathValidator) UpdateHerdrSockets(scope string, sockets []string) {
	if scope == "" {
		scope = LocateScopeFull
	}
	set := make(map[string]struct{}, len(sockets))
	for _, s := range sockets {
		if len(set) >= maxTrackedDirs {
			break
		}
		if !isCleanAbsPath(s) {
			continue
		}
		set[s] = struct{}{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if strings.HasPrefix(scope, "tree:") {
		if _, exists := v.herdrBuckets[scope]; !exists && len(v.herdrBuckets) >= maxTreeAuthBuckets+3 {
			for k := range v.herdrBuckets {
				if strings.HasPrefix(k, "tree:") {
					delete(v.herdrBuckets, k)
					break
				}
			}
		}
	}
	v.herdrBuckets[scope] = set
}

// HasHerdrSocket reports whether sock is a clean absolute path that appeared in
// an active locate bucket's discovered HERDR_SOCKET_PATH set.
func (v *PathValidator) HasHerdrSocket(sock string) bool {
	if !isCleanAbsPath(sock) {
		return false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, set := range v.herdrBuckets {
		if _, ok := set[sock]; ok {
			return true
		}
	}
	return false
}

// KnownHerdrSockets returns a deduplicated snapshot of currently tracked
// located herdr socket paths across all scopes.
func (v *PathValidator) KnownHerdrSockets() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, set := range v.herdrBuckets {
		for s := range set {
			seen[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out
}

func isCleanAbsPath(p string) bool {
	if p == "" || !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\n") {
		return false
	}
	return filepath.Clean(p) == p
}

func inferLogCap(p string) string {
	switch {
	case strings.HasSuffix(p, "/opencode/opencode.db"):
		return proto.ReadAgentLogsOpenCode
	case strings.HasSuffix(p, "/devin/cli/sessions.db"):
		return proto.ReadAgentLogsDevin
	case strings.Contains(p, "/brain/") && strings.HasSuffix(p, "/transcript_full.jsonl"):
		return proto.ReadAgentLogsAgy
	case strings.Contains(p, "/sessions/") && strings.Contains(filepath.Base(p), "rollout-"):
		return proto.ReadAgentLogsCodex
	default:
		return proto.ReadAgentLogsClaude
	}
}

func (v *PathValidator) allowedRootsLocked() []allowedRoot {
	roots := make([]allowedRoot, 0, 16)
	if v.home != "" {
		roots = append(roots,
			allowedRoot{dir: filepath.Join(v.home, ".claude", "projects"), cap: proto.ReadAgentLogsClaude},
			allowedRoot{dir: filepath.Join(v.home, ".codex", "sessions"), cap: proto.ReadAgentLogsCodex},
			allowedRoot{dir: filepath.Join(v.home, ".gemini", "antigravity-cli", "brain"), cap: proto.ReadAgentLogsAgy},
			allowedRoot{dir: filepath.Join(v.home, ".local", "share", "opencode"), cap: proto.ReadAgentLogsOpenCode},
			allowedRoot{dir: filepath.Join(v.home, ".local", "share", "devin", "cli"), cap: proto.ReadAgentLogsDevin},
		)
	}
	if v.selfClaudeDir != "" {
		roots = append(roots, allowedRoot{dir: filepath.Join(v.selfClaudeDir, "projects"), cap: proto.ReadAgentLogsClaude})
	}
	if v.xdgDataHome != "" {
		roots = append(roots,
			allowedRoot{dir: filepath.Join(v.xdgDataHome, "opencode"), cap: proto.ReadAgentLogsOpenCode},
			allowedRoot{dir: filepath.Join(v.xdgDataHome, "devin", "cli"), cap: proto.ReadAgentLogsDevin},
		)
	}
	addBucketDirs := func(b *authBucket) {
		if b == nil {
			return
		}
		for d := range b.claudeCfgDirs {
			roots = append(roots, allowedRoot{dir: filepath.Join(d, "projects"), cap: proto.ReadAgentLogsClaude})
		}
		for d := range b.codexHomes {
			roots = append(roots, allowedRoot{dir: filepath.Join(d, "sessions"), cap: proto.ReadAgentLogsCodex})
		}
	}
	addBucketDirs(v.fullBucket)
	addBucketDirs(v.opencodeBucket)
	for _, b := range v.treeBuckets {
		addBucketDirs(b)
	}
	return roots
}

// authorizePathLocked determines whether literal path string p is authorized
// without ever consulting EvalSymlinks. Returns (capability, ok).
func (v *PathValidator) authorizePathLocked(p string) (string, bool) {
	if v.fullBucket != nil {
		if capCat, ok := v.fullBucket.knownLogs[p]; ok {
			return capCat, true
		}
	}
	if v.opencodeBucket != nil {
		if capCat, ok := v.opencodeBucket.knownLogs[p]; ok {
			return capCat, true
		}
	}
	for _, b := range v.treeBuckets {
		if capCat, ok := b.knownLogs[p]; ok {
			b.lastUsed.Store(v.useSeq.Add(1))
			return capCat, true
		}
	}
	for _, root := range v.allowedRootsLocked() {
		if isStrictSubpath(root.dir, p) {
			return root.cap, true
		}
	}
	return "", false
}

// openNoFollowChain opens fullPath component-by-component from "/" using
// openat(O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC) for every parent directory
// and openat(O_RDONLY|O_NOFOLLOW|O_CLOEXEC|O_NONBLOCK) for the final file.
// This eliminates TOCTOU symlink-swap races without EvalSymlinks or Lstat.
func openNoFollowChain(fullPath string) (*os.File, error) {
	if !isCleanAbsPath(fullPath) || fullPath == "/" {
		return nil, errors.New("invalid path")
	}
	parts := strings.Split(strings.TrimPrefix(fullPath, "/"), "/")
	if len(parts) == 0 {
		return nil, errors.New("empty path")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("invalid path segment")
		}
	}

	dirFD, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	for i := 0; i < len(parts)-1; i++ {
		nextFD, err := syscall.Openat(dirFD, parts[i], syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(dirFD)
		if err != nil {
			return nil, err
		}
		dirFD = nextFD
	}

	leaf := parts[len(parts)-1]
	fileFD, err := syscall.Openat(dirFD, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	_ = syscall.Close(dirFD)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fileFD), fullPath), nil
}

// OpenChecked authorizes literal path string against the current locate set or
// declared reads directories, opens every component with openat + O_NOFOLLOW,
// and verifies via fstat that the opened file is a regular file owned by v.uid.
// Returns (nil, "", false) if any check fails.
func (v *PathValidator) OpenChecked(path string) (*os.File, string, bool) {
	if !isCleanAbsPath(path) {
		return nil, "", false
	}

	v.mu.RLock()
	capCat, authorized := v.authorizePathLocked(path)
	v.mu.RUnlock()
	if !authorized {
		return nil, "", false
	}

	assertDeclaredRead(capCat)
	f, err := openNoFollowChain(path)
	if err != nil {
		return nil, "", false
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, "", false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != v.uid {
		_ = f.Close()
		return nil, "", false
	}
	return f, capCat, true
}

// ValidatePath reports whether path passes PathValidator.OpenChecked without
// keeping the file open.
func (v *PathValidator) ValidatePath(path string) (string, bool) {
	f, capCat, ok := v.OpenChecked(path)
	if !ok {
		return "", false
	}
	_ = f.Close()
	return capCat, true
}

func isStrictSubpath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// Tails reads the last 16 KiB of each validated log file, producing output
// byte-for-byte identical to convoTailsCommand. Disallowed or missing paths are
// skipped silently, matching `[ -f "$l" ] || continue`.
func Tails(v *PathValidator, logs []string) []byte {
	var out bytes.Buffer
	for _, l := range logs {
		f, capCat, ok := v.OpenChecked(l)
		if !ok {
			continue
		}
		assertDeclaredRead(capCat)
		out.WriteString(l)
		out.WriteByte('\n')
		tail, err := readTailFromFile(f, convoTailBytes)
		_ = f.Close()
		if err != nil {
			out.WriteByte(0x01)
		} else {
			out.Write(tail)
		}
		out.WriteByte(0)
	}
	return out.Bytes()
}

// MessageTails reads a bounded 4 MiB window at the end of each validated log
// file, filters for short (<= 65536 byte) message and turn-boundary JSON
// records, and keeps at most the last 64 KiB per log — byte-for-byte identical
// to convoMessageTailsCommand under LC_ALL=C.
func MessageTails(v *PathValidator, logs []string) []byte {
	var out bytes.Buffer
	for _, l := range logs {
		f, capCat, ok := v.OpenChecked(l)
		if !ok {
			continue
		}
		assertDeclaredRead(capCat)
		out.WriteString(l)
		out.WriteByte('\n')
		window, err := readTailFromFile(f, convoMsgTailWindowBytes)
		_ = f.Close()
		if err != nil {
			out.WriteByte(0x01)
			out.WriteByte(0)
			continue
		}
		out.Write(filterMessageTailWindow(window))
		out.WriteByte(0)
	}
	return out.Bytes()
}

// filterMessageTailWindow mirrors the awk + `tail -c 65536` pipeline in
// convoMessageTailsCommand, measuring line length in bytes (conservative bound
// matching LC_ALL=C).
func filterMessageTailWindow(window []byte) []byte {
	var filtered bytes.Buffer
	bad := false
	rem := window
	for len(rem) > 0 {
		nl := bytes.IndexByte(rem, '\n')
		var line []byte
		if nl < 0 {
			line = rem
			rem = nil
		} else {
			line = rem[:nl]
			rem = rem[nl+1:]
		}
		if bytes.IndexByte(line, 0x01) >= 0 {
			bad = true
			continue
		}
		if len(line) <= convoMsgTailMaxLineBytes && messageTailTypeRe.Match(line) {
			filtered.Write(line)
			filtered.WriteByte('\n')
		}
	}
	if bad {
		filtered.WriteByte(0x01)
	}
	res := filtered.Bytes()
	if len(res) > convoMsgTailOutputBytes {
		res = res[len(res)-convoMsgTailOutputBytes:]
	}
	return res
}
