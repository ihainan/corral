//go:build linux

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	convoTailBytes           = 16384
	convoMsgTailWindowBytes  = 4194304 // 4 MiB
	convoMsgTailOutputBytes  = 65536   // 64 KiB
	convoMsgTailMaxLineRunes = 65536
)

var messageTailTypeRe = regexp.MustCompile(`"type"[ ]*:[ ]*"(message|user_message|agent_message|item_completed|custom_tool_call|function_call|task_started|task_complete|thread_rolled_back)"`)

// PathValidator restricts log file reads to regular files owned by the daemon's
// UID that either appeared in current locate results or reside under a declared
// reads agent log directory without following symlinks outside that directory.
type PathValidator struct {
	home string
	uid  uint32
	rd   *capReader

	mu             sync.RWMutex
	knownRealLogs  map[string]string // realPath -> capability category
	claudeCfgDirs  map[string]struct{}
	codexHomes     map[string]struct{}
	xdgDataHome    string
	selfClaudeDir  string
	explicitAllow  []allowedRoot // optional test-only roots
}

type allowedRoot struct {
	dir string
	cap string
}

// NewPathValidator creates a PathValidator for (home, uid).
func NewPathValidator(home string, uid uint32, procRoot string) *PathValidator {
	rd := newCapReader(procRoot)
	return &PathValidator{
		home:          home,
		uid:           uid,
		rd:            rd,
		knownRealLogs: make(map[string]string),
		claudeCfgDirs: make(map[string]struct{}),
		codexHomes:    make(map[string]struct{}),
	}
}

// SetEnvDirs overrides the default self CLAUDE_CONFIG_DIR and XDG_DATA_HOME
// used when computing declared agent log root directories.
func (v *PathValidator) SetEnvDirs(claudeCfgDir, xdgDataHome string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.selfClaudeDir = claudeCfgDir
	v.xdgDataHome = xdgDataHome
}

// UpdateDiscovered records log paths and custom agent config directories seen
// during the most recent locate scan.
func (v *PathValidator) UpdateDiscovered(logs, claudeDirs, codexHomes []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, p := range logs {
		if p == "" || !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\n") {
			continue
		}
		capCat := inferLogCap(p)
		if realPath, err := v.rd.evalSymlinks(capCat, p); err == nil && realPath != "" {
			v.knownRealLogs[realPath] = capCat
		}
	}
	for _, d := range claudeDirs {
		if d != "" && filepath.IsAbs(d) {
			v.claudeCfgDirs[d] = struct{}{}
		}
	}
	for _, d := range codexHomes {
		if d != "" && filepath.IsAbs(d) {
			v.codexHomes[d] = struct{}{}
		}
	}
}

// KnownLogs returns a snapshot of all currently known located real log paths.
func (v *PathValidator) KnownLogs() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]string, 0, len(v.knownRealLogs))
	for p := range v.knownRealLogs {
		out = append(out, p)
	}
	return out
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
	roots := make([]allowedRoot, 0, 8+len(v.claudeCfgDirs)+len(v.codexHomes))
	if v.home != "" {
		roots = append(roots,
			allowedRoot{dir: filepath.Join(v.home, ".claude", "projects"), cap: proto.ReadAgentLogsClaude},
			allowedRoot{dir: filepath.Join(v.home, ".codex", "sessions"), cap: proto.ReadAgentLogsCodex},
			allowedRoot{dir: filepath.Join(v.home, ".gemini", "antigravity-cli", "brain"), cap: proto.ReadAgentLogsAgy},
			allowedRoot{dir: filepath.Join(v.home, ".local", "share", "opencode"), cap: proto.ReadAgentLogsOpenCode},
			allowedRoot{dir: filepath.Join(v.home, ".local", "share", "devin", "cli"), cap: proto.ReadAgentLogsDevin},
		)
	}
	if v.selfClaudeDir != "" && filepath.IsAbs(v.selfClaudeDir) {
		roots = append(roots, allowedRoot{dir: filepath.Join(v.selfClaudeDir, "projects"), cap: proto.ReadAgentLogsClaude})
	}
	if v.xdgDataHome != "" && filepath.IsAbs(v.xdgDataHome) {
		roots = append(roots,
			allowedRoot{dir: filepath.Join(v.xdgDataHome, "opencode"), cap: proto.ReadAgentLogsOpenCode},
			allowedRoot{dir: filepath.Join(v.xdgDataHome, "devin", "cli"), cap: proto.ReadAgentLogsDevin},
		)
	}
	for d := range v.claudeCfgDirs {
		roots = append(roots, allowedRoot{dir: filepath.Join(d, "projects"), cap: proto.ReadAgentLogsClaude})
	}
	for d := range v.codexHomes {
		roots = append(roots, allowedRoot{dir: filepath.Join(d, "sessions"), cap: proto.ReadAgentLogsCodex})
	}
	roots = append(roots, v.explicitAllow...)
	return roots
}

// OpenChecked validates path against all security constraints and returns an
// already-opened *os.File (opened with O_NOFOLLOW to avoid TOCTOU races) and
// the declared proto.ReadAgentLogs* capability category. Returns (nil, "", false)
// if path is disallowed, missing, a symlink, not a regular file, or owned by
// another UID.
func (v *PathValidator) OpenChecked(path string) (*os.File, string, bool) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n") {
		return nil, "", false
	}

	// Open leaf without following a leaf symlink; O_NONBLOCK prevents blocking
	// forever if path is a FIFO/named pipe before fstat rejects non-regular files.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", false
	}
	f := os.NewFile(uintptr(fd), path)
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

	// Resolve parent directory symlinks so a directory symlink escaping an
	// allowed root (e.g. ~/.claude/projects/escape -> /etc) is detected.
	realParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || realParent == "" {
		_ = f.Close()
		return nil, "", false
	}
	realPath := filepath.Join(realParent, filepath.Base(path))

	// Confirm the opened FD still matches the resolved real path (guards
	// against symlink swap between Open and EvalSymlinks).
	realInfo, err := os.Lstat(realPath)
	if err != nil || !realInfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, "", false
	}
	realSt, ok := realInfo.Sys().(*syscall.Stat_t)
	if !ok || realSt.Dev != st.Dev || realSt.Ino != st.Ino || realSt.Uid != v.uid {
		_ = f.Close()
		return nil, "", false
	}

	// Authorize against either (1) current locate results or (2) declared reads
	// agent log root directories.
	v.mu.RLock()
	capCat, known := v.knownRealLogs[realPath]
	roots := v.allowedRootsLocked()
	v.mu.RUnlock()

	if known {
		assertDeclaredRead(capCat)
		return f, capCat, true
	}

	for _, root := range roots {
		realRoot, err := v.rd.evalSymlinks(root.cap, root.dir)
		if err != nil || realRoot == "" {
			continue
		}
		if isStrictSubpath(realRoot, realPath) {
			assertDeclaredRead(root.cap)
			return f, root.cap, true
		}
	}

	_ = f.Close()
	return nil, "", false
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
// file, filters for short message and turn-boundary JSON records, and keeps at
// most the last 64 KiB per log — byte-for-byte identical to
// convoMessageTailsCommand.
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
// convoMessageTailsCommand.
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
		if utf8.RuneCount(line) <= convoMsgTailMaxLineRunes && messageTailTypeRe.Match(line) {
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
