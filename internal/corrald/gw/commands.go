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
	opIDRE  = regexp.MustCompile(`^[0-9a-f]{8,32}$`)
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
	FailedJSON    string
	OpsDir        string
	TombstonePath string
	SocketPath    string
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

// MkStageCommand creates the state directory (0700) and a private stage
// directory (0700) for uploading corrald.
func MkStageCommand(stateDir, stageDir string) string {
	return shellScript(`umask 077
mkdir -p "$ST" && chmod 700 "$ST" || { echo "error=mkdir"; exit 1; }
if [ -e "$ST/removing" ]; then echo "error=removing"; exit 1; fi
mkdir -p "$STAGE" && chmod 700 "$STAGE" || { echo "error=stage"; exit 1; }
echo "result=ok"
`, "ST", stateDir, "STAGE", stageDir)
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

// ServeCommand starts corrald detached via `<bin> serve --gateway <gw>`.
func ServeCommand(binPath, gateway string) string {
	return shellScript(`if [ ! -x "$BIN" ]; then echo "error=not_installed"; exit 1; fi
"$BIN" serve --gateway "$GW"
`, "BIN", binPath, "GW", gateway)
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
	return shellScript(`if [ -d "$ST" ]; then
  : > "$ST/removing" || { echo "error=tombstone_failed"; exit 1; }
fi
echo "result=ok"
`, "ST", stateDir)
}

// UninstallCommand runs `<bin> uninstall [--op <id>]`. If the current symlink
// is missing (e.g. partial install or damaged symlink), it attempts to locate any
// corrald-*/corrald binary before falling back to safe shell cleanup.
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
# Binary missing: safe fallback cleanup of state dir and corrald-* entries only.
if [ -d "$ST" ]; then : > "$ST/removing" 2>/dev/null || true; fi
if [ -n "$SOCK" ]; then rm -f "$SOCK" 2>/dev/null || true; fi
rm -rf "$ST" 2>/dev/null || true
SR=$(dirname "$ST")
others=0
if [ -d "$SR" ]; then
  for d in "$SR"/*; do
    if [ -d "$d" ] && [ -f "$d/install.json" ]; then others=1; break; fi
  done
fi
if [ "$others" = "0" ]; then
  rm -rf "$L/corrald" "$L"/corrald-* "$L/README.txt" 2>/dev/null || true
  rmdir "$SR" 2>/dev/null || true
  rmdir "$L" 2>/dev/null || true
fi
if [ -e "$ST" ]; then
  echo "result=partial"
  echo "left=$ST	failed to remove"
  exit 1
fi
echo "result=removed"
`, "L", libDir, "ST", stateDir, "SOCK", sockPath, "OP", opID)
}

// SwitchSymlinkCommand atomically updates ~/.local/lib/corral/corrald to point
// to relTarget (used when rolling back to a previous version after crash loop).
func SwitchSymlinkCommand(libDir, relTarget string) string {
	return shellScript(`TMP="$L/.corrald.rb.$$"
rm -f "$TMP"
ln -s "$REL" "$TMP" && mv -Tf "$TMP" "$L/corrald" || { rm -f "$TMP"; echo "error=symlink_failed"; exit 1; }
echo "result=ok"
`, "L", libDir, "REL", relTarget)
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
