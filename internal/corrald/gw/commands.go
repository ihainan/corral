package gw

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ihainan/corral/internal/sshx"
)

// CommandTag marks $0 on every remote corrald helper command so it is easy to
// identify in process lists and logs.
const CommandTag = "corral-corrald"

// MinFreeKB is the minimum free space required in $HOME to install or upgrade
// corrald (~30 MB, matching consent-design.md).
var MinFreeKB int64 = 30 << 10

// MaxSocketPath matches daemon.MaxSocketPath (Unix sun_path headroom).
const MaxSocketPath = 99

var (
	hex12RE = regexp.MustCompile(`^[0-9a-f]{12}$`)
	uidRE   = regexp.MustCompile(`^[0-9]{1,10}$`)
	opIDRE  = regexp.MustCompile(`^[0-9A-Za-z_-]{8,96}$`)
)

// ArchMap maps `uname -m` values to Go/distribution architecture names.
var ArchMap = map[string]string{
	"x86_64":  "amd64",
	"amd64":   "amd64",
	"aarch64": "arm64",
	"arm64":   "arm64",
}

func shellScript(body string, vars ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(vars); i += 2 {
		b.WriteString(vars[i] + "=" + sshx.QuoteShell(vars[i+1]) + "\n")
	}
	b.WriteString(body)
	return "sh -c " + sshx.QuoteShell(b.String()) + " " + CommandTag
}

// ParseKeyValues parses key=value lines (keeping the first value for each key).
func ParseKeyValues(out []byte) map[string]string {
	m := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			if _, seen := m[k]; !seen {
				m[k] = v
			}
		}
	}
	return m
}

// ParseLeftLines extracts `left=<path>\t<reason>` lines from uninstall output.
func ParseLeftLines(out []byte) []LeftItem {
	var items []LeftItem
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "left=") {
			continue
		}
		rest := strings.TrimPrefix(line, "left=")
		p, reason, _ := strings.Cut(rest, "\t")
		p = strings.TrimSpace(p)
		if p != "" {
			items = append(items, LeftItem{Path: p, Reason: strings.TrimSpace(reason)})
		}
	}
	return items
}

// LeftItem is a path that could not be deleted during uninstall.
type LeftItem struct {
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

// HostProbe holds the parsed result of ProbeCommand.
type HostProbe struct {
	OS            string // uname -s: Linux, Darwin, ...
	Machine       string // uname -m: x86_64, aarch64, ...
	Arch          string // mapped: amd64, arm64, or ""
	UID           string
	Home          string
	FreeKB        int64 // -1 if unknown
	MachineID     string
	MachineIDSrc  string
	HomeWritable  bool
	HasTombstone  bool
	HasSymlink    bool
	HasInstall    bool
	CurrentTarget string
	Stages        []string
	Versions      []string

	// Computed paths mirroring internal/corrald/daemon/paths.go.
	LibDir        string
	CurrentBin    string
	StateRoot     string
	StateDir      string
	InstallJSON   string
	MetaJSON      string
	ClientsJSON   string
	FailedJSON    string
	OpsDir        string
	TombstonePath string
	SocketPath    string
}

// UIDInt64 returns the parsed numeric UID or -1 if unavailable.
func (p *HostProbe) UIDInt64() int64 {
	if p == nil || !uidRE.MatchString(p.UID) {
		return -1
	}
	n, err := strconv.ParseInt(p.UID, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// ProbeCommand inspects the target host without writing permanent files:
// OS, arch, uid, home, free space, machine id, writability, existing install,
// tombstone, version directories, and leftover stage directories.
func ProbeCommand() string {
	return shellScript(`echo "os=$(uname -s 2>/dev/null)"
echo "machine=$(uname -m 2>/dev/null)"
echo "uid=$(id -u 2>/dev/null)"
echo "home=$HOME"
echo "free_kb=$(df -Pk "$HOME" 2>/dev/null | awk 'NR==2 {print $4}')"
mid=
mid_src=
if [ -r /etc/machine-id ]; then
  mid=$(head -n 1 /etc/machine-id 2>/dev/null | tr -d ' \t\r\n-')
  mid_src=/etc/machine-id
elif [ -r /var/lib/dbus/machine-id ]; then
  mid=$(head -n 1 /var/lib/dbus/machine-id 2>/dev/null | tr -d ' \t\r\n-')
  mid_src=/var/lib/dbus/machine-id
fi
if [ -z "$mid" ]; then
  mid_src=hostname
  echo "hostname=$(hostname 2>/dev/null)"
else
  echo "mid_raw=$mid"
fi
echo "mid_src=$mid_src"
if [ -w "$HOME" ]; then
  echo "home_writable=1"
else
  echo "home_writable=0"
fi
L="$HOME/.local/lib/corral"
if [ -e "$L/corrald" ] || [ -L "$L/corrald" ]; then
  echo "has_symlink=1"
  tgt=$(readlink "$L/corrald" 2>/dev/null || true)
  echo "current_target=$tgt"
fi
if [ -d "$L" ]; then
  for d in "$L"/corrald-*; do
    [ -d "$d" ] && echo "version_dir=$(basename "$d")"
  done
fi
echo "result=ok"
`)
}

// ProbeStateCommand inspects the machine-id specific state directory once mid
// has been resolved (checking tombstone, install.json, leftover stage-* dirs).
func ProbeStateCommand(stateDir string) string {
	return shellScript(`if [ -e "$ST/removing" ]; then echo "tombstone=1"; fi
if [ -f "$ST/install.json" ]; then echo "has_install=1"; fi
if [ -d "$ST" ]; then
  for d in "$ST"/stage-*; do
    [ -e "$d" ] && echo "stage_dir=$(basename "$d")"
  done
fi
echo "result=ok"
`, "ST", stateDir)
}

// ParseProbe parses ProbeCommand output and computes all canonical host paths.
func ParseProbe(out []byte) (*HostProbe, error) {
	f := ParseKeyValues(out)
	if f["result"] != "ok" {
		return nil, fmt.Errorf("host probe did not complete")
	}
	p := &HostProbe{
		OS:            strings.TrimSpace(f["os"]),
		Machine:       strings.TrimSpace(f["machine"]),
		UID:           strings.TrimSpace(f["uid"]),
		Home:          strings.TrimSpace(f["home"]),
		FreeKB:        -1,
		HomeWritable:  f["home_writable"] == "1",
		HasSymlink:    f["has_symlink"] == "1",
		CurrentTarget: strings.TrimSpace(f["current_target"]),
	}
	p.Arch = ArchMap[p.Machine]
	if n, err := strconv.ParseInt(strings.TrimSpace(f["free_kb"]), 10, 64); err == nil {
		p.FreeKB = n
	}

	// Collect all version_dir lines.
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "version_dir="); ok && v != "" {
			p.Versions = append(p.Versions, v)
		}
	}

	// Resolve machine id identically to daemon.ResolveMachineID.
	midRaw := strings.ToLower(strings.TrimSpace(f["mid_raw"]))
	if len(midRaw) >= 12 && hex12RE.MatchString(midRaw[:12]) {
		p.MachineID = midRaw[:12]
		p.MachineIDSrc = f["mid_src"]
	} else {
		host := strings.TrimSpace(f["hostname"])
		if host != "" {
			sum := sha256.Sum256([]byte(host))
			p.MachineID = hex.EncodeToString(sum[:6])
			p.MachineIDSrc = "hostname"
		}
	}

	if p.Home != "" && strings.HasPrefix(p.Home, "/") && !strings.ContainsAny(p.Home, "\x00\n") && p.MachineID != "" {
		home := strings.TrimRight(p.Home, "/")
		if home == "" {
			home = "/"
		}
		p.LibDir = home + "/.local/lib/corral"
		p.CurrentBin = p.LibDir + "/corrald"
		p.StateRoot = home + "/.local/state/corral/corrald"
		p.StateDir = p.StateRoot + "/" + p.MachineID
		p.InstallJSON = p.StateDir + "/install.json"
		p.MetaJSON = p.StateDir + "/meta.json"
		p.ClientsJSON = p.StateDir + "/clients.json"
		p.FailedJSON = p.StateDir + "/failed.json"
		p.OpsDir = p.StateDir + "/ops"
		p.TombstonePath = p.StateDir + "/removing"
		p.SocketPath = ComputeSocketPath(home, p.UID, p.MachineID)
	}
	return p, nil
}

// ComputeSocketPath mirrors daemon.ResolveLayout's socket path computation.
func ComputeSocketPath(home, uid, mid string) string {
	home = strings.TrimRight(home, "/")
	sock := home + "/.local/state/corral/corrald/" + mid + "/corrald.sock"
	if len(sock) <= MaxSocketPath {
		return sock
	}
	sum := sha256.Sum256([]byte(mid + home))
	return fmt.Sprintf("/tmp/corrald-%s-%s/corrald.sock", uid, hex.EncodeToString(sum[:4]))
}

// MkStageCommand safely creates the state directory (0700) and a private stage
// directory (0700) step-by-step, refusing any symlink or foreign-owned component
// along ~/.local/lib/corral and ~/.local/state/corral/corrald/<mid>/stage-<op>.
func MkStageCommand(home, stateDir, stageDir string) string {
	return shellScript(`umask 077
ME=$(id -u)
check_or_mk() {
  p="$1"
  mode="$2"
  if [ -L "$p" ]; then
    echo "error=unsafe_path"
    echo "detail=symlink at $p"
    exit 1
  fi
  if [ -e "$p" ]; then
    if [ ! -d "$p" ]; then
      echo "error=unsafe_path"
      echo "detail=not a directory: $p"
      exit 1
    fi
  else
    mkdir "$p" || { echo "error=mkdir"; exit 1; }
  fi
  if [ -L "$p" ] || [ ! -d "$p" ]; then
    echo "error=unsafe_path"
    exit 1
  fi
  own=$(stat -c %u "$p" 2>/dev/null || echo "")
  if [ -n "$own" ] && [ "$own" != "$ME" ]; then
    echo "error=unsafe_path"
    echo "detail=unexpected owner $own on $p"
    exit 1
  fi
  if [ -n "$mode" ]; then
    chmod "$mode" "$p" || { echo "error=chmod"; exit 1; }
  fi
}
check_existing_dir() {
  p="$1"
  if [ -L "$p" ]; then
    echo "error=unsafe_path"
    echo "detail=symlink at $p"
    exit 1
  fi
  if [ -e "$p" ]; then
    if [ ! -d "$p" ]; then
      echo "error=unsafe_path"
      exit 1
    fi
    own=$(stat -c %u "$p" 2>/dev/null || echo "")
    if [ -n "$own" ] && [ "$own" != "$ME" ]; then
      echo "error=unsafe_path"
      echo "detail=unexpected owner $own on $p"
      exit 1
    fi
  fi
}
H="${HOME_DIR%/}"
check_existing_dir "$H/.local"
check_existing_dir "$H/.local/lib"
check_existing_dir "$H/.local/lib/corral"
check_or_mk "$H/.local" ""
check_or_mk "$H/.local/state" ""
check_or_mk "$H/.local/state/corral" ""
check_or_mk "$H/.local/state/corral/corrald" ""
check_or_mk "$ST" "700"
if [ -e "$ST/removing" ]; then echo "error=removing"; exit 1; fi
check_or_mk "$STAGE" "700"
echo "result=ok"
`, "HOME_DIR", home, "ST", stateDir, "STAGE", stageDir)
}

// RmStageCommand removes a staging directory.
func RmStageCommand(stageDir string) string {
	return shellScript(`rm -rf -- "$STAGE" || { echo "error=rm_failed"; exit 1; }
echo "result=ok"
`, "STAGE", stageDir)
}

// SelftestCommand executes `<bin> selftest` and classifies execution failures
// (e.g. noexec / permission denied returning exit 126).
func SelftestCommand(binPath string) string {
	return shellScript(`chmod 700 "$BIN" || { echo "error=chmod_failed"; exit 1; }
"$BIN" selftest
rc=$?
if [ $rc -eq 126 ]; then
  echo "error=noexec"
  exit 126
fi
exit $rc
`, "BIN", binPath)
}

// InstallCommand executes `<bin> install ...` from the staging directory.
func InstallCommand(binPath, stageDir, opID, gateway, by string, upgrade bool) string {
	upFlag := ""
	if upgrade {
		upFlag = " --upgrade"
	}
	return shellScript(`"$BIN" install --stage "$STAGE" --op "$OP" --gateway "$GW" --by "$BY"`+upFlag+`
`, "BIN", binPath, "STAGE", stageDir, "OP", opID, "GW", gateway, "BY", by)
}

// ServeCommand starts corrald detached via `<bin> serve --gateway <gw>`,
// passing `--tmux <path>` when `command -v tmux` resolves a binary on the host.
// If corrald rejects the discovered tmux path with error=invalid_tmux (e.g.
// untrusted directory), ServeCommand retries without --tmux so the daemon still
// starts cleanly and tmux queries fall back to SSH.
func ServeCommand(binPath, gateway string) string {
	return shellScript(`if [ ! -x "$BIN" ]; then echo "error=not_installed"; exit 1; fi
TMUX_BIN=$(command -v tmux 2>/dev/null || true)
if [ -n "$TMUX_BIN" ]; then
  OUT=$("$BIN" serve --gateway "$GW" --tmux "$TMUX_BIN")
  RC=$?
  if [ $RC -ne 0 ] && printf '%s\n' "$OUT" | grep -q '^error=invalid_tmux$'; then
    "$BIN" serve --gateway "$GW"
    exit $?
  fi
  printf '%s\n' "$OUT"
  exit $RC
fi
"$BIN" serve --gateway "$GW"
`, "BIN", binPath, "GW", gateway)
}

// VersionCommand runs `<bin> version` to inspect an existing installation.
func VersionCommand(binPath string) string {
	return shellScript(`if [ ! -x "$BIN" ]; then echo "error=not_installed"; exit 1; fi
"$BIN" version
`, "BIN", binPath)
}

// StatusCommand runs `<bin> status`.
func StatusCommand(binPath string) string {
	return shellScript(`if [ ! -x "$BIN" ]; then echo "result=ok"; echo "state=not_installed"; exit 0; fi
"$BIN" status
`, "BIN", binPath)
}

// StopCommand runs `<bin> stop`.
func StopCommand(binPath string) string {
	return shellScript(`if [ ! -x "$BIN" ]; then echo "result=ok"; echo "already_stopped=true"; exit 0; fi
"$BIN" stop
`, "BIN", binPath)
}

// WriteTombstoneCommand writes the removing tombstone to block concurrent starts.
func WriteTombstoneCommand(stateDir string) string {
	return shellScript(`if [ -d "$ST" ] && [ ! -L "$ST" ]; then
  : > "$ST/removing" || { echo "error=tombstone_failed"; exit 1; }
fi
echo "result=ok"
`, "ST", stateDir)
}

// UninstallCommand runs `<bin> uninstall [--op <id>]`. If the current symlink
// and all versioned binaries are missing, the fallback verifies meta.json and
// corrald.lock, stops any running process (verifying /proc/<pid>/stat start
// time), and refuses to delete state files (returning error=stop_failed) if
// the process or lock cannot be stopped.
func UninstallCommand(libDir, stateDir, sockPath, opID string) string {
	return shellScript(`BIN="$L/corrald"
if [ ! -x "$BIN" ]; then
  for cand in "$L"/corrald-*/corrald; do
    if [ -x "$cand" ]; then BIN="$cand"; break; fi
  done
fi
if [ -x "$BIN" ]; then
  if [ -n "$OP" ]; then
    "$BIN" uninstall --op "$OP"
  else
    "$BIN" uninstall
  fi
  exit $?
fi
# Binary missing: write tombstone, check meta.json + lock, stop verified process first.
if [ -d "$ST" ] && [ ! -L "$ST" ]; then : > "$ST/removing" 2>/dev/null || true; fi

proc_start() {
  p="$1"
  [ -r "/proc/$p/stat" ] || return 1
  awk '{ i = index($0, ") "); if (i == 0) exit 1; rest = substr($0, i+2); n = split(rest, a, " "); if (n < 20) exit 1; if (a[1] == "Z" || a[1] == "X") exit 1; print a[20] }' "/proc/$p/stat" 2>/dev/null
}

proc_matches() {
  p="$1"
  st="$2"
  case "$p" in ''|*[!0-9]*) return 1 ;; esac
  [ "$p" -gt 0 ] 2>/dev/null || return 1
  kill -0 "$p" 2>/dev/null || return 1
  cur=$(proc_start "$p") || return 1
  if [ -n "$st" ]; then
    [ "$cur" = "$st" ] || return 1
  fi
  return 0
}

stop_verified() {
  p="$1"
  st="$2"
  proc_matches "$p" "$st" || return 0
  kill -TERM "$p" 2>/dev/null || true
  i=0
  while [ $i -lt 30 ]; do
    proc_matches "$p" "$st" || return 0
    sleep 0.1
    i=$((i + 1))
  done
  proc_matches "$p" "$st" || return 0
  kill -KILL "$p" 2>/dev/null || true
  i=0
  while [ $i -lt 20 ]; do
    proc_matches "$p" "$st" || return 0
    sleep 0.1
    i=$((i + 1))
  done
  proc_matches "$p" "$st" && return 1
  return 0
}

meta_pid=0
meta_st=
prev_pid=0
prev_st=
if [ -f "$ST/meta.json" ] && [ ! -L "$ST/meta.json" ]; then
  meta_pid=$(sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$ST/meta.json" | head -n 1)
  meta_st=$(sed -n 's/.*"start_time"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$ST/meta.json" | head -n 1)
  prev_pid=$(sed -n 's/.*"prev_pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$ST/meta.json" | head -n 1)
  prev_st=$(sed -n 's/.*"prev_start_time"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$ST/meta.json" | head -n 1)
fi

if [ -n "$meta_pid" ] && [ "$meta_pid" -gt 0 ] 2>/dev/null; then
  if ! stop_verified "$meta_pid" "$meta_st"; then
    echo "error=stop_failed"
    echo "pid=$meta_pid"
    echo "detail=process did not exit after SIGTERM and SIGKILL"
    exit 1
  fi
fi
if [ -n "$prev_pid" ] && [ "$prev_pid" -gt 0 ] 2>/dev/null; then
  if ! stop_verified "$prev_pid" "$prev_st"; then
    echo "error=stop_failed"
    echo "pid=$prev_pid"
    echo "detail=previous process did not exit after SIGTERM and SIGKILL"
    exit 1
  fi
fi

# Verify corrald.lock is no longer held by any live process before deleting state dir.
if [ -f "$ST/corrald.lock" ] && command -v flock >/dev/null 2>&1; then
  locked=1
  i=0
  while [ $i -lt 10 ]; do
    if flock -n "$ST/corrald.lock" true 2>/dev/null; then
      locked=0
      break
    fi
    sleep 0.1
    i=$((i + 1))
  done
  if [ "$locked" = "1" ]; then
    echo "error=stop_failed"
    echo "pid=${meta_pid:-0}"
    echo "detail=corrald.lock is still held by an active process"
    exit 1
  fi
fi

if [ -n "$SOCK" ]; then rm -f "$SOCK" 2>/dev/null || true; fi
rm -rf "$ST" 2>/dev/null || true
SR=$(dirname "$ST")
others=0
if [ -d "$SR" ]; then
  for d in "$SR"/*; do
    if [ -d "$d" ] && [ -f "$d/install.json" ]; then others=1; break; fi
  done
fi
SC=$(dirname "$SR")
if [ "$others" = "0" ]; then
  rm -rf "$L/corrald" "$L"/corrald-* "$L/README.txt" 2>/dev/null || true
  rmdir "$SR" 2>/dev/null || true
  if [ -d "$SC/run" ] && [ ! -L "$SC/run" ] && [ -O "$SC/run" ]; then
    rm -rf "$SC/run" 2>/dev/null || true
  fi
  rmdir "$SC" 2>/dev/null || true
  rmdir "$L" 2>/dev/null || true
fi
left_any=0
if [ -e "$ST" ]; then
  left_any=1
  echo "left=$ST	failed to remove"
fi
if [ "$others" = "0" ] && [ -d "$SC/run" ] && [ ! -L "$SC/run" ] && [ -O "$SC/run" ]; then
  left_any=1
  echo "left=$SC/run	corrald run dir still exists after removal"
fi
if [ "$others" = "0" ] && [ -d "$L" ]; then
  for rem in "$L/corrald" "$L/README.txt" "$L"/corrald-*; do
    if [ -e "$rem" ] || [ -L "$rem" ]; then
      left_any=1
      echo "left=$rem	corrald file still exists after removal"
    fi
  done
fi
if [ "$left_any" = "1" ]; then
  echo "result=partial"
  exit 1
fi
echo "result=removed"
`, "L", libDir, "ST", stateDir, "SOCK", sockPath, "OP", opID)
}

// StartCandidate describes the single authoritative binary chosen by
// SelectStartBinary to launch via `corrald serve`.
type StartCandidate struct {
	Path      string // absolute binary path on the host
	Version   string // parsed version (e.g. "0.3.2") when known
	Release   int64  // parsed release (> 0) when known
	RelTarget string // relative target under libDir (e.g. "corrald-0.3.2-2000/corrald")
}

// StartSelectOptions configures SelectStartBinary.
type StartSelectOptions struct {
	// Rollback allows selecting a version lower than the shared symlink target
	// (e.g. post-update rollback or explicit replace). Failed releases are
	// still strictly excluded.
	Rollback bool
	// BelowRelease, when > 0 in Rollback mode, restricts selection to installed
	// non-failed releases strictly lower than BelowRelease (computing the
	// rollback target dynamically from host state rather than pre-storing one).
	BelowRelease int64
	// PreferredTarget (e.g. "corrald-0.3.1-1000/corrald") is used only for
	// explicit replace mode where a specific version directory is requested.
	PreferredTarget string
}

// parseVersionDirInfo extracts (version, release, relTarget) from a bare
// "corrald-<ver>-<rel>" directory name (no path separators or traversal).
func parseVersionDirInfo(name string) (ver string, rel int64, relTarget string, ok bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return "", 0, "", false
	}
	if !strings.HasPrefix(name, "corrald-") {
		return "", 0, "", false
	}
	rest := strings.TrimPrefix(name, "corrald-")
	idx := strings.LastIndexByte(rest, '-')
	if idx <= 0 || idx+1 >= len(rest) {
		return "", 0, "", false
	}
	ver = rest[:idx]
	rel, err := strconv.ParseInt(rest[idx+1:], 10, 64)
	if err != nil || rel <= 0 || ver == "" {
		return "", 0, "", false
	}
	return ver, rel, name + "/corrald", true
}

// parseSymlinkTargetInLibDir validates that a shared symlink target points
// strictly to a versioned binary `<dir>/corrald` directly inside libDir
// (rejecting relative traversal like "../outside/..." or absolute paths outside
// libDir).
func parseSymlinkTargetInLibDir(libDir, target string) (ver string, rel int64, relTarget string, ok bool) {
	target = strings.TrimSpace(target)
	if target == "" || strings.ContainsAny(target, "\x00\\") {
		return "", 0, "", false
	}
	if strings.HasPrefix(target, "/") {
		cleanLib := strings.TrimRight(strings.TrimSpace(libDir), "/")
		if cleanLib == "" || !strings.HasPrefix(target, cleanLib+"/") {
			return "", 0, "", false
		}
		target = strings.TrimPrefix(target, cleanLib+"/")
	}
	// Strip optional leading "./" once.
	target = strings.TrimPrefix(target, "./")
	if !strings.HasSuffix(target, "/corrald") {
		return "", 0, "", false
	}
	dir := strings.TrimSuffix(target, "/corrald")
	return parseVersionDirInfo(dir)
}

// SelectStartBinary is the single authoritative selector for deciding which
// corrald binary to start on a host (first start, auto-restart, retry,
// cold-start update, and rollback).
//
// Invariants:
//  1. ALWAYS returns the immutable versioned binary path inside
//     `<libDir>/corrald-<ver>-<rel>/corrald`, NEVER the mutable shared symlink
//     path (`probe.CurrentBin`), preventing an NFS peer from mutating the
//     symlink target between selection and execution.
//  2. Only selects candidates whose version directory actually exists in
//     `probe.Versions` inside `probe.LibDir`. A symlink target outside `LibDir`
//     or pointing to a missing version directory is treated as non-existent
//     (never selected and never blacklisted).
//  3. Never selects any release present in failedReleases.
//  4. In normal mode (!opts.Rollback), never selects a version lower than a
//     valid, existing, non-failed shared symlink target.
//  5. In rollback mode (opts.Rollback), selects opts.PreferredTarget if present
//     in `probe.Versions` and not failed, otherwise the highest installed
//     non-failed release (strictly `< opts.BelowRelease` when `BelowRelease > 0`).
//  6. Returns ok=false if no eligible non-failed versioned binary exists.
func SelectStartBinary(probe *HostProbe, failedReleases []int64, opts StartSelectOptions) (StartCandidate, bool) {
	if probe == nil || probe.LibDir == "" {
		return StartCandidate{}, false
	}
	failed := make(map[int64]bool, len(failedReleases))
	for _, fr := range failedReleases {
		if fr > 0 {
			failed[fr] = true
		}
	}

	var installed []StartCandidate
	for _, dir := range probe.Versions {
		ver, rel, relTarget, ok := parseVersionDirInfo(dir)
		if !ok {
			continue
		}
		installed = append(installed, StartCandidate{
			Path:      probe.LibDir + "/" + relTarget,
			Version:   ver,
			Release:   rel,
			RelTarget: relTarget,
		})
	}

	highestNonFailed := func(minRel, belowRel int64) (StartCandidate, bool) {
		var best StartCandidate
		found := false
		for _, c := range installed {
			if failed[c.Release] || c.Release < minRel {
				continue
			}
			if belowRel > 0 && c.Release >= belowRel {
				continue
			}
			if !found || c.Release > best.Release {
				best = c
				found = true
			}
		}
		return best, found
	}

	if opts.Rollback {
		if opts.PreferredTarget != "" {
			if _, prefRel, prefRelTarget, ok := parseSymlinkTargetInLibDir(probe.LibDir, opts.PreferredTarget); ok && !failed[prefRel] && (opts.BelowRelease <= 0 || prefRel < opts.BelowRelease) {
				for _, c := range installed {
					if c.RelTarget == prefRelTarget {
						return c, true
					}
				}
			}
		}
		return highestNonFailed(1, opts.BelowRelease)
	}

	// Normal mode: resolve the shared symlink target strictly within LibDir and
	// verify that the target version directory actually exists in probe.Versions.
	// Targets outside LibDir or missing from LibDir are treated as non-existent.
	_, curRel, curRelTarget, hasCur := parseSymlinkTargetInLibDir(probe.LibDir, probe.CurrentTarget)
	if probe.HasSymlink && hasCur {
		if !failed[curRel] {
			for _, c := range installed {
				if c.RelTarget == curRelTarget {
					return c, true
				}
			}
			// Target version directory does not exist in LibDir: fall through
			// to select from remaining healthy installed versions.
		} else {
			// Shared symlink points to a blacklisted release: prefer any non-failed
			// version >= curRel first, then fall back to the highest non-failed
			// installed version (NFS post-rollback case).
			if best, ok := highestNonFailed(curRel, 0); ok {
				return best, true
			}
			return highestNonFailed(1, 0)
		}
	}

	return highestNonFailed(1, 0)
}

// FriendlyOSName maps uname -s output to user-facing OS names for failure text.
func FriendlyOSName(unameOS string) string {
	switch strings.ToLower(strings.TrimSpace(unameOS)) {
	case "darwin":
		return "macOS"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	case "netbsd":
		return "NetBSD"
	default:
		return strings.TrimSpace(unameOS)
	}
}
