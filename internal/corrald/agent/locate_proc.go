//go:build linux

package agent

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const (
	// atClkTck is Linux auxiliary vector tag AT_CLKTCK (17).
	atClkTck = 17
)

var (
	logTSRe = regexp.MustCompile(`"(timestamp|created_at)" *: *"20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9:.]*Z"`)
)

// procStat holds the fields extracted from /proc/<pid>/stat.
type procStat struct {
	ppid  string
	start string
	fg    string // "1" or "0"
}

func (l *Locator) readProcStat(pid string) (procStat, bool) {
	raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, pid, "stat"))
	if err != nil {
		return procStat{}, false
	}
	return parseStatBytes(raw)
}

// parseStatBytes mirrors convoLocateWith's stat_of and ppid_of: joins lines
// with ' ', slices after the last ") ", and extracts ppid ($2), pgrp ($3),
// tpgid ($6), and starttime ($20).
func parseStatBytes(raw []byte) (procStat, bool) {
	s := strings.ReplaceAll(string(raw), "\n", " ")
	idx := strings.LastIndex(s, ") ")
	if idx < 0 {
		return procStat{}, false
	}
	fields := strings.Fields(s[idx+2:])
	if len(fields) < 20 {
		return procStat{}, false
	}
	ppid := fields[1]
	pgrp := fields[2]
	tpgid := fields[5]
	st := fields[19]
	if !isAllDigits(st) {
		return procStat{}, false
	}
	fg := "0"
	if pgrp != "" && pgrp == tpgid {
		fg = "1"
	}
	return procStat{ppid: ppid, start: st, fg: fg}, true
}

func (l *Locator) ppidOf(pid string) string {
	raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, pid, "stat"))
	if err != nil {
		return ""
	}
	s := strings.ReplaceAll(string(raw), "\n", " ")
	idx := strings.LastIndex(s, ") ")
	if idx < 0 {
		return ""
	}
	fields := strings.Fields(s[idx+2:])
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// readCommRaw reads /proc/<pid>/comm and strips trailing newlines (matching
// `read_all "$d/comm" "$nl"` followed by trailing newline strip).
func (l *Locator) readCommRaw(pid string) (string, bool) {
	raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, pid, "comm"))
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(raw), "\n"), true
}

// readCommAnc reads /proc/<pid>/comm for the anc= field: joins lines with ""
// and sanitizes non-[A-Za-z0-9._+-] bytes after capping at 32 bytes.
func (l *Locator) readCommAnc(pid string) string {
	raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, pid, "comm"))
	if err != nil {
		return ""
	}
	c := strings.ReplaceAll(string(raw), "\n", "")
	needClean := false
	for i := 0; i < len(c); i++ {
		if !isAncCommByte(c[i]) {
			needClean = true
			break
		}
	}
	if !needClean {
		return c
	}
	if len(c) > 32 {
		c = c[:32]
	}
	buf := make([]byte, len(c))
	for i := 0; i < len(c); i++ {
		if isAncCommByte(c[i]) {
			buf[i] = c[i]
		} else {
			buf[i] = '_'
		}
	}
	return string(buf)
}

func isAncCommByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '.' || b == '_' || b == '+' || b == '-'
}

func (l *Locator) readCmdlineArgs(pid string) []string {
	raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, pid, "cmdline"))
	if err != nil || len(raw) == 0 {
		return nil
	}
	// Strip a single trailing NUL (standard Linux /proc/<pid>/cmdline format) so
	// we match awk RS="\001" behavior where a trailing record separator does not
	// produce an extra empty record.
	if raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	parts := bytes.Split(raw, []byte{0})
	args := make([]string, len(parts))
	for i, p := range parts {
		args[i] = string(p)
	}
	return args
}

func (l *Locator) linkOf(path string, maxBytes int) string {
	target, err := l.rd.readLink(proto.ReadProc, path)
	if err != nil {
		return ""
	}
	if len(target) > maxBytes {
		target = target[:maxBytes]
	}
	if strings.IndexByte(target, '\n') >= 0 {
		target = strings.ReplaceAll(target, "\n", "")
	}
	return target
}

func (l *Locator) readBtime() string {
	raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, "stat"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok && isAllDigits(rest) {
			return rest
		}
	}
	return ""
}

// readClkTck reads AT_CLKTCK (17) from /proc/self/auxv without cgo or spawning
// getconf. Falls back to 100 (standard Linux USER_HZ) if auxv is unreadable.
func (l *Locator) readClkTck() string {
	raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, "self", "auxv"))
	if err == nil && len(raw) >= 16 {
		for i := 0; i+16 <= len(raw); i += 16 {
			tag := binary.LittleEndian.Uint64(raw[i : i+8])
			val := binary.LittleEndian.Uint64(raw[i+8 : i+16])
			if tag == atClkTck && val > 0 {
				return strconv.FormatUint(val, 10)
			}
			if tag == 0 {
				break
			}
		}
	}
	return "100"
}

// listProcPIDs returns all numeric PID directory names under /proc sorted
// lexicographically (matching shell glob /proc/[0-9]*).
func (l *Locator) listProcPIDs() []string {
	names, err := l.rd.readDirNames(proto.ReadProc, l.procRoot)
	if err != nil {
		return nil
	}
	pids := make([]string, 0, len(names))
	for _, name := range names {
		if isAllDigits(name) {
			pids = append(pids, name)
		}
	}
	sort.Strings(pids)
	return pids
}

// listFDLinks returns symlink targets for /proc/<pid>/fd/* sorted by fd name
// lexicographically (matching shell glob "$1"/fd/*).
func (l *Locator) listFDLinks(pidDir string) []string {
	fdDir := filepath.Join(pidDir, "fd")
	names, err := l.rd.readDirNames(proto.ReadProc, fdDir)
	if err != nil {
		return nil
	}
	sort.Strings(names)
	links := make([]string, 0, len(names))
	for _, name := range names {
		target, err := l.rd.readLink(proto.ReadProc, filepath.Join(fdDir, name))
		if err != nil {
			continue
		}
		links = append(links, target)
	}
	return links
}

func (l *Locator) buildAncestors(pid string) string {
	var b strings.Builder
	a := pid
	for i := 0; i < 40; i++ {
		pp := l.ppidOf(a)
		if pp == "" || pp == "0" || pp == "1" {
			break
		}
		a = pp
		b.WriteString(a)
		b.WriteByte(':')
		b.WriteString(l.readCommAnc(a))
		b.WriteByte(',')
	}
	return b.String()
}

// readRecEnv reads the four pane-ownership environment variables for rec():
// HERDR_PANE_ID, HERDR_SOCKET_PATH, TMUX, TMUX_PANE.
func (l *Locator) readRecEnv(pid string) string {
	kvs, err := l.rd.readProcEnv(
		filepath.Join(l.procRoot, pid),
		proto.ReadEnvHerdrPaneID,
		proto.ReadEnvHerdrSocketPath,
		proto.ReadEnvTmux,
		proto.ReadEnvTmuxPane,
	)
	if err != nil || len(kvs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, kv := range kvs {
		b.WriteString(kv.key)
		b.WriteByte('=')
		b.WriteString(kv.val)
		b.WriteByte('\n')
		if b.Len() >= 4096 {
			break
		}
	}
	s := b.String()
	if len(s) > 4096 {
		s = s[:4096]
	}
	// Command substitution `env=$(...)` strips all trailing newlines.
	return strings.TrimRight(s, "\n")
}

// readLogTS computes the `logts` field when times=="times" for a .jsonl log:
// latest `"timestamp"` / `"created_at"` in the last 256 KiB not ahead of now.
func (l *Locator) readLogTS(cap, logPath string, now time.Time) string {
	data, err := l.rd.readTail(cap, logPath, 262144)
	if err != nil || len(data) == 0 {
		return ""
	}
	nowNorm := now.UTC().Format("2006-01-02T15:04:05") + ".999999999"
	matches := logTSRe.FindAll(data, -1)
	bestNorm := ""
	bestOrig := ""
	for _, m := range matches {
		// cut -d'"' -f4 extracts the 4th quote-delimited field: the timestamp value.
		parts := bytes.Split(m, []byte{'"'})
		if len(parts) < 4 {
			continue
		}
		orig := string(parts[3])
		norm := normalizeLogTS(orig)
		if norm <= nowNorm && norm > bestNorm {
			bestNorm = norm
			bestOrig = orig
		}
	}
	return bestOrig
}

func normalizeLogTS(s string) string {
	t := strings.TrimSuffix(s, "Z")
	dot := strings.IndexByte(t, '.')
	if dot < 0 {
		t += "."
		dot = len(t) - 1
	}
	frac := t[dot+1:] + "000000000"
	return t[:dot+1] + frac[:9]
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func isUUIDOrHyphenChars(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '-') {
			return false
		}
	}
	return true
}

func isVersionComm(comm string) bool {
	// Matches shell glob `[0-9]*.[0-9]*`: first byte is a digit, contains '.',
	// and some '.' is immediately followed by a digit.
	if len(comm) < 3 || comm[0] < '0' || comm[0] > '9' {
		return false
	}
	for i := 1; i+1 < len(comm); i++ {
		if comm[i] == '.' && comm[i+1] >= '0' && comm[i+1] <= '9' {
			return true
		}
	}
	return false
}

func agentLogCap(agent string) string {
	switch agent {
	case "claude":
		return proto.ReadAgentLogsClaude
	case "codex":
		return proto.ReadAgentLogsCodex
	case "opencode":
		return proto.ReadAgentLogsOpenCode
	case "devin":
		return proto.ReadAgentLogsDevin
	case "agy":
		return proto.ReadAgentLogsAgy
	default:
		return proto.ReadAgentLogsClaude
	}
}
