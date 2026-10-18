// Package codeserver builds the remote commands that install, start and stop
// code-server (VS Code in the browser) on a target host, and reads their
// answers.
//
// code-server is never shipped with Corral. It lives under the SSH account:
//
//	~/.local/lib/corral/code-server-<version>/   the release, unpacked
//	~/.local/share/corral/code-server/           user data and extensions
//	~/.local/state/corral/                       pid file, log (and socket)
//
// It listens only on a Unix socket (mode 600) that the gateway reaches over
// the SSH connection; no TCP port is opened on the host.
//
// Every command is a fixed script; the only values in it (version, arch,
// URL, paths) go through sshx.QuoteShell. Scripts answer with key=value
// lines; "result=..." on success, "error=<code>" (and "detail=...") on failure.
package codeserver

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ihainan/corral/internal/sshx"
)

// Tag ends every command ($0 of the script), so it is easy to recognise.
const Tag = "corral-code"

// MaxSocketPath keeps the socket path under the Unix limit (108 bytes on
// Linux, 104 elsewhere) with room to spare.
const MaxSocketPath = 99

// MinFreeKB is the space an install needs in the home directory (the
// archive and the unpacked release, with headroom).
var MinFreeKB int64 = 800 << 10

var (
	versionRE = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)
	uidRE     = regexp.MustCompile(`^[0-9]{1,10}$`)
)

// Arches maps `uname -m` to the release's architecture name.
var Arches = map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

func script(body string, vars ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(vars); i += 2 {
		b.WriteString(vars[i] + "=" + sshx.QuoteShell(vars[i+1]) + "\n")
	}
	b.WriteString(body)
	return "sh -c " + sshx.QuoteShell(b.String()) + " " + Tag
}

const layout = `L="$HOME/.local/lib/corral"
DATA="$HOME/.local/share/corral/code-server"
ST="$HOME/.local/state/corral"
`

// Probe is what the gateway needs to know about the host.
type Probe struct {
	Arch       string // release name: amd64, arm64
	Machine    string // `uname -m` as reported
	UID        string
	Home       string
	Curl, Wget bool
	Tar        bool
	FreeKB     int64  // -1 when unknown
	Installed  bool   // this version is unpacked and runnable
	Runtime    string // $XDG_RUNTIME_DIR, if the session has one
}

// ProbeCommand reports the host's architecture, account, tools, free space
// and whether this version is installed.
func ProbeCommand(version string) string {
	return script(layout+`echo "machine=$(uname -m)"
echo "uid=$(id -u)"
echo "home=$HOME"
command -v curl >/dev/null 2>&1 && echo curl=1
command -v wget >/dev/null 2>&1 && echo wget=1
command -v tar >/dev/null 2>&1 && echo tar=1
echo "free=$(df -Pk "$HOME" 2>/dev/null | awk 'NR==2 {print $4}')"
echo "runtime=${XDG_RUNTIME_DIR:-}"
[ -x "$L/code-server-$V/bin/code-server" ] && echo installed=1
echo result=ok
`, "V", version)
}

// Fields parses key=value answer lines (the first value of each key).
func Fields(out []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if _, seen := m[k]; ok && !seen {
			m[k] = v
		}
	}
	return m
}

var ErrArch = errors.New("unsupported architecture")

// ParseProbe reads ProbeCommand's answer (its Fields).
func ParseProbe(f map[string]string) (*Probe, error) {
	if f["result"] != "ok" {
		return nil, errors.New("the host did not answer the check")
	}
	p := &Probe{Machine: f["machine"], UID: f["uid"], Home: f["home"], Curl: f["curl"] == "1", Wget: f["wget"] == "1",
		Tar: f["tar"] == "1", Installed: f["installed"] == "1", FreeKB: -1, Runtime: f["runtime"]}
	if n, err := strconv.ParseInt(f["free"], 10, 64); err == nil {
		p.FreeKB = n
	}
	if !uidRE.MatchString(p.UID) || !strings.HasPrefix(p.Home, "/") || strings.ContainsAny(p.Home, "\x00\n") {
		return nil, errors.New("the host reported an unusable account (user id or home directory)")
	}
	arch, ok := Arches[p.Machine]
	if !ok {
		return p, fmt.Errorf("%w %q (code-server runs on x86_64 and aarch64)", ErrArch, p.Machine)
	}
	p.Arch = arch
	return p, nil
}

// SocketPath is where code-server listens: in the state directory under
// home; when that path is too long for a socket, in $XDG_RUNTIME_DIR/corral
// (runtime, if the host has one); otherwise in a private directory under
// /tmp named after the user id and home. (The home directory comes first: the
// runtime directory disappears when the account's last session ends, which
// would take the socket of a code-server that keeps running with it.) The
// start command checks the directory is a real directory of the account
// with mode 0700 before using it.
func SocketPath(home, uid, runtime string) (string, error) {
	if !uidRE.MatchString(uid) || !strings.HasPrefix(home, "/") {
		return "", errors.New("unusable user id or home directory")
	}
	if p := strings.TrimSuffix(home, "/") + "/.local/state/corral/code-server.sock"; len(p) <= MaxSocketPath {
		return p, nil
	}
	if strings.HasPrefix(runtime, "/") && !strings.ContainsAny(runtime, "\x00\n") {
		if p := strings.TrimSuffix(runtime, "/") + "/corral/code-server.sock"; len(p) <= MaxSocketPath {
			return p, nil
		}
	}
	sum := sha256.Sum256([]byte(home))
	return "/tmp/corral-" + uid + "-" + hex.EncodeToString(sum[:4]) + "/code-server.sock", nil
}

// UploadPath is where an uploaded release goes, relative to the home
// directory (SFTP paths start there).
func UploadPath(version, arch string) string {
	return ".local/lib/corral/.upload-" + version + "-" + arch + ".tar.gz"
}

// DownloadPath is where a downloaded release goes, relative to the home directory.
func DownloadPath(version, arch string) string {
	return ".local/lib/corral/.download-" + version + "-" + arch + ".tar.gz"
}

// PrepareCommand creates the library directory (before an SFTP upload).
func PrepareCommand() string {
	return script(layout + `umask 077
mkdir -p "$L" && chmod 700 "$L" || { echo error=mkdir; exit 1; }
echo result=ok
`)
}

// DownloadCommand fetches the release on the host with curl or wget.
func DownloadCommand(version, arch, url string) string {
	return script(layout+`umask 077
mkdir -p "$L" && chmod 700 "$L" || { echo error=mkdir; exit 1; }
F="$HOME/$P"
rm -f "$F" "$F.err"
# HTTPS only, redirects included.
if command -v curl >/dev/null 2>&1; then
  curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 20 --retry 2 -o "$F" "$U" 2>"$F.err"; rc=$?
elif command -v wget >/dev/null 2>&1; then
  wget -q --https-only -T 20 -t 2 -O "$F" "$U" 2>"$F.err"; rc=$?
else
  echo error=nodownloader; exit 1
fi
if [ "$rc" != 0 ]; then
  echo error=download
  echo "detail=$(tail -c 300 "$F.err" 2>/dev/null | tr '\n' ' ')"
  rm -f "$F" "$F.err"; exit 1
fi
rm -f "$F.err"
echo result=ok
`, "V", version, "A", arch, "U", url, "P", DownloadPath(version, arch))
}

// lock serialises the scripts that touch the installation and the running
// instance (unpack, start, stop) on the host itself, where flock exists.
const lock = `mkdir -p "$ST" && chmod 700 "$ST" || { echo error=mkdir; exit 1; }
if command -v flock >/dev/null 2>&1; then exec 9>"$ST/.lock" && flock -w 120 9 || { echo error=locked; exit 1; }; fi
`

// ExtractCommand checks an archive (relative to home) against its trusted
// SHA-256 digest, unpacks it into a fresh temporary directory without taking
// owners from it, checks the release's entry point is a real executable file,
// and renames the release into place, so a half-unpacked or unexpected
// release is never used. The archive is removed either way.
func ExtractCommand(version, arch, archive, sha256 string) string {
	return script(layout+lock+`umask 077
D="$L/code-server-$V"
F="$HOME/$P"
if [ -x "$D/bin/code-server" ]; then rm -f "$F"; echo result=ok; exit 0; fi
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$F" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$F" | cut -d' ' -f1)
else echo error=nosha; rm -f "$F"; exit 1; fi
if [ "$got" != "$H" ]; then echo error=checksum; echo "detail=$got"; rm -f "$F"; exit 1; fi
T=$(mktemp -d "$L/.extract-XXXXXX") || { echo error=mktemp; rm -f "$F"; exit 1; }
if ! tar --no-same-owner -xzf "$F" -C "$T" 2>"$T.err"; then
  echo error=extract
  echo "detail=$(tail -c 300 "$T.err" 2>/dev/null | tr '\n' ' ')"
  rm -rf "$T" "$T.err" "$F"; exit 1
fi
rm -f "$F" "$T.err"
S="$T/code-server-$V-linux-$A"
E="$S/bin/code-server"
if [ -L "$S" ] || [ ! -d "$S" ] || [ -L "$S/bin" ] || [ ! -d "$S/bin" ] || [ -L "$E" ] || [ ! -f "$E" ] || [ ! -x "$E" ]; then
  echo error=layout; rm -rf "$T"; exit 1
fi
[ -e "$D" ] || mv "$S" "$D"
rm -rf "$T"
if [ ! -x "$D/bin/code-server" ]; then echo error=install; exit 1; fi
echo result=ok
`, "V", version, "A", arch, "P", archive, "H", sha256)
}

// instance reads the pid file: the process id, its start time (field 22 of
// /proc/<pid>/stat, so a reused process id is told apart) and the instance
// id Corral gave it. alive is true when that very process still runs (and
// is not a zombie).
const instance = `P="$ST/code-server.pid"
starttime() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }
running() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | awk -v st="$2" '$1 != "Z" && $20 == st {ok=1} END {exit !ok}'; }
alive() { [ -f "$P" ] && read -r pid st id <"$P" && [ -n "$pid" ] && [ -n "$st" ] && running "$pid" "$st"; }
`

// socketDir makes sure the socket's directory is a real directory of this
// account with mode 0700: created with that mode when missing (never through
// an existing name), and refused when it is a symbolic link, someone else's,
// or open to others. Under /tmp nothing is created on the way there.
const socketDir = `SD=$(dirname "$S")
case "$SD" in /tmp/*) ;; *) mkdir -p "$(dirname "$SD")" || { echo error=socketdir; exit 1; } ;; esac
[ -e "$SD" ] || [ -L "$SD" ] || mkdir -m 0700 "$SD" 2>/dev/null
if [ -L "$SD" ] || [ ! -d "$SD" ] || [ ! -O "$SD" ] || [ "$(stat -c %a "$SD" 2>/dev/null)" != 700 ]; then
  echo error=socketdir; echo "detail=$SD is not a private directory of this account"; exit 1
fi
`

// StartCommand starts code-server detached on the socket, or reports that it
// already runs. It waits for the socket to appear. instanceID is recorded in
// the pid file with the process id and start time.
func StartCommand(version, socket, instanceID string) string {
	return script(layout+instance+lock+`umask 077
D="$L/code-server-$V"
mkdir -p "$DATA/extensions" && chmod 700 "$HOME/.local/share/corral" "$DATA" || { echo error=mkdir; exit 1; }
`+socketDir+`if alive && [ -S "$S" ]; then echo result=running; echo "instance=$id"; exit 0; fi
[ -x "$D/bin/code-server" ] || { echo error=notinstalled; exit 1; }
rm -f "$S"
SETSID=; command -v setsid >/dev/null 2>&1 && SETSID=setsid
$SETSID nohup "$D/bin/code-server" --socket "$S" --socket-mode 600 --auth none --disable-telemetry \
  --disable-update-check --disable-proxy --disable-getting-started-override \
  --user-data-dir "$DATA" --extensions-dir "$DATA/extensions" \
  >"$ST/code-server.log" 2>&1 </dev/null 9>&- & # (it must not keep the lock)
new=$!
echo "$new $(starttime "$new") $I" >"$P"
i=0
while [ $i -lt 30 ]; do
  [ -S "$S" ] && { echo result=started; echo "instance=$I"; exit 0; }
  kill -0 "$new" 2>/dev/null || break # (its command line is in flux while it starts)
  sleep 1; i=$((i+1))
done
echo error=start
echo "detail=$(tail -n 5 "$ST/code-server.log" 2>/dev/null | tr '\n' ' ' | cut -c1-300)"
exit 1
`, "V", version, "S", socket, "I", instanceID)
}

// StopCommand stops the code-server named by the pid file (its whole
// process group), but only that very process: same id and start time. It
// fails, and leaves the state in place, when it cannot read the pid file or
// the process survives.
func StopCommand(socket string) string {
	return script(layout+instance+lock+`if [ ! -f "$P" ]; then rm -f "$S"; echo result=ok; exit 0; fi
if ! read -r pid st id <"$P" || [ -z "$pid" ] || [ -z "$st" ]; then echo error=verify; exit 1; fi
if alive; then
  kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null
  i=0
  while alive && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done
  if alive; then kill -9 -- "-$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null; sleep 1; fi
  if alive; then echo error=kill; exit 1; fi
fi
rm -f "$P" "$S"
echo result=ok
`, "S", socket)
}

// ValidVersion reports whether a version can be put into paths and commands.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }
