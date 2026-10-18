// Package codeserver builds the remote commands that install, start and stop
// code-server (VS Code in the browser) on a target host, and reads their
// answers.
//
// code-server is never shipped with Corral. It lives under the SSH account:
//
//	~/.local/lib/corral/code-server-<version>/   the release, unpacked
//	~/.local/share/corral/code-server/           user data and extensions
//	~/.local/state/corral/                       pid file, log (and socket)
//	~/.local/state/corral/install-<task>/        one install's private staging
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
	taskRE    = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ValidTask reports whether a task or instance id can be put into names.
func ValidTask(id string) bool { return taskRE.MatchString(id) }

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
	Runtime    string // $XDG_RUNTIME_DIR, if the session has one and it is a private directory of the account
	Flock      bool   // the host has flock and setsid, which installing and starting need
	Setsid     bool
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
X="${XDG_RUNTIME_DIR:-}"
if [ -n "$X" ] && [ ! -L "$X" ] && [ -d "$X" ] && [ -O "$X" ] && [ "$(stat -c %a "$X" 2>/dev/null)" = 700 ]; then echo "runtime=$X"; fi
command -v flock >/dev/null 2>&1 && echo flock=1
command -v setsid >/dev/null 2>&1 && echo setsid=1
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
		Tar: f["tar"] == "1", Installed: f["installed"] == "1", FreeKB: -1, Runtime: f["runtime"],
		Flock: f["flock"] == "1", Setsid: f["setsid"] == "1"}
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

// lock serialises the scripts that touch the installation and the running
// instance (install, start, stop) on the host. flock is required.
const lock = `mkdir -p "$ST" && chmod 700 "$ST" || { echo error=mkdir; exit 1; }
command -v flock >/dev/null 2>&1 || { echo error=noflock; exit 1; }
command -v setsid >/dev/null 2>&1 || { echo error=nosetsid; exit 1; }
exec 9>"$ST/.lock" && flock -w 120 9 || { echo error=locked; exit 1; }
`

// Source of the release archive for InstallCommand.
const (
	FromDownload = "download" // the host downloads it from the URL
	FromStdin    = "stdin"    // the gateway sends it on the command's input
)

// installWork runs in its own session and process group (setsid), so a
// cancelled install can be ended as a whole (CancelInstallCommand). It
// records its process id and start time in the staging directory, gets the
// archive into it, checks the digest, checks every member's path and link
// target stays inside the release's top directory, unpacks without taking
// owners from the archive, checks the entry point is a regular executable
// file, and renames the release into place. It reports its phase as it goes.
const installWork = `me=$(sed 's/^.*) //' /proc/$$/stat 2>/dev/null)
[ "$(echo "$me" | cut -d' ' -f3)" = "$$" ] || { echo error=nosetsid; exit 1; }
echo "$$ $(echo "$me" | cut -d' ' -f20)" >"$STAGE/task.pid"
F="$STAGE/archive.tar.gz"
cancelled() { [ -e "$ST/cancel-$T" ]; }
if [ "$M" = stdin ]; then
  echo phase=uploading
  cat >"$F" || { echo error=upload; exit 1; }
else
  echo phase=downloading
  # HTTPS only, redirects included.
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 20 --retry 2 -o "$F" "$U" 2>"$STAGE/err"; rc=$?
  elif command -v wget >/dev/null 2>&1; then
    wget -q --https-only -T 20 -t 2 -O "$F" "$U" 2>"$STAGE/err"; rc=$?
  else
    echo error=nodownloader; exit 1
  fi
  if [ "$rc" != 0 ]; then echo error=download; echo "detail=$(tail -c 300 "$STAGE/err" 2>/dev/null | tr '\n' ' ')"; exit 1; fi
fi
cancelled && { echo error=cancelled; exit 1; }
echo phase=checking
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum <"$F") || { echo error=nosha; exit 1; }
elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 <"$F") || { echo error=nosha; exit 1; }
else echo error=nosha; exit 1; fi
got=${got%% *}
[ "$got" = "$H" ] || { echo error=checksum; echo "detail=$got"; exit 1; }
cancelled && { echo error=cancelled; exit 1; }
echo phase=extracting
TOP="code-server-$V-linux-$A"
tar -tzf "$F" >"$STAGE/names" 2>/dev/null && tar -tvzf "$F" >"$STAGE/list" 2>/dev/null || { echo error=extract; exit 1; }
inside='function ok(p,  n, a, i, d) { if (p ~ /^\//) return 0; n = split(p, a, "/"); d = 0
  for (i = 1; i <= n; i++) { if (a[i] == "" || a[i] == ".") continue; if (a[i] == "..") { if (--d < 1) return 0 }
    else { if (d == 0 && a[i] != top) return 0; d++ } }
  return d >= 1 }'
awk -v top="$TOP" "$inside"' { if (!ok($0)) bad = 1 } END { exit bad }' "$STAGE/names" || { echo error=members; exit 1; }
awk -v top="$TOP" "$inside"' /^[lh]/ { line = $0; for (k = 0; k < 5; k++) sub(/^[^ ]+ +/, "", line)
  if ($1 ~ /^l/) { i = index(line, " -> "); name = substr(line, 1, i - 1); t = substr(line, i + 4)
    d = name; sub(/\/[^\/]*\/?$/, "", d); if (i == 0 || t ~ /^\// || !ok(d "/" t)) bad = 1 }
  else { i = index(line, " link to "); t = substr(line, i + 9); if (i == 0 || !ok(t)) bad = 1 } } END { exit bad }' "$STAGE/list" ||
  { echo error=members; exit 1; }
mkdir -m 0700 "$STAGE/x" && tar --no-same-owner -xzf "$F" -C "$STAGE/x" 2>"$STAGE/err" ||
  { echo error=extract; echo "detail=$(tail -c 300 "$STAGE/err" 2>/dev/null | tr '\n' ' ')"; exit 1; }
S="$STAGE/x/$TOP"
E="$S/bin/code-server"
if [ -L "$S" ] || [ ! -d "$S" ] || [ -L "$S/bin" ] || [ ! -d "$S/bin" ] || [ -L "$E" ] || [ ! -f "$E" ] || [ ! -x "$E" ]; then
  echo error=layout; exit 1
fi
cancelled && { echo error=cancelled; exit 1; }
[ -e "$D" ] || mv "$S" "$D"
[ -x "$D/bin/code-server" ] || { echo error=install; exit 1; }
echo result=ok
`

// InstallCommand installs a release on the host in one go, holding the
// host's lock throughout: everything happens in the task's own staging
// directory (created afresh, mode 0700, removed on every exit), so neither
// a concurrent install nor anything else can change the archive between its
// digest check and unpacking. With FromStdin the archive is read from the
// command's input. task must satisfy ValidTask.
func InstallCommand(task, version, arch, from, url, sha256 string) string {
	return script(layout+lock+`umask 077
D="$L/code-server-$V"
STAGE="$ST/install-$T"
find "$ST" -maxdepth 1 -name 'cancel-*' -mtime +1 -exec rm -f {} + 2>/dev/null
if [ -e "$ST/cancel-$T" ]; then rm -f "$ST/cancel-$T"; echo error=cancelled; exit 1; fi
if [ -x "$D/bin/code-server" ]; then [ "$M" = stdin ] && cat >/dev/null; echo result=ok; exit 0; fi
mkdir -p "$L" && chmod 700 "$L" || { echo error=mkdir; exit 1; }
mkdir -m 0700 "$STAGE" || { echo error=stage; exit 1; }
trap 'rm -rf "$STAGE"; rm -f "$ST/cancel-$T"' EXIT
trap 'exit 1' HUP INT TERM
export L ST D STAGE T V A M U H
setsid sh -c "$W" corral-code-install
# (Should setsid have had to fork, wait for the install itself.)
if read -r wp _ <"$STAGE/task.pid" 2>/dev/null; then while kill -0 "$wp" 2>/dev/null; do sleep 1; done; fi
`, "T", task, "V", version, "A", arch, "M", from, "U", url, "H", sha256, "W", installWork)
}

// CancelInstallCommand ends a cancelled install: it marks the task cancelled
// (so an install still waiting for the lock gives up), ends the task's
// process group and waits until it is gone, then removes its staging
// directory. It reports an error when the group survives.
func CancelInstallCommand(task string) string {
	return script(layout+`STAGE="$ST/install-$T"
mkdir -p "$ST" 2>/dev/null && : >"$ST/cancel-$T"
if [ -f "$STAGE/task.pid" ] && read -r pid st <"$STAGE/task.pid" && [ -n "$pid" ] &&
   [ "$(sed 's/^.*) //' "/proc/$pid/stat" 2>/dev/null | cut -d' ' -f20)" = "$st" ]; then
  kill -TERM "-$pid" 2>/dev/null
  i=0
  while kill -0 "-$pid" 2>/dev/null && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done
  kill -KILL "-$pid" 2>/dev/null
  i=0
  while kill -0 "-$pid" 2>/dev/null && [ $i -lt 5 ]; do sleep 1; i=$((i+1)); done
  if kill -0 "-$pid" 2>/dev/null; then echo error=kill; exit 1; fi
fi
rm -rf "$STAGE"
rm -f "$L"/.upload-* "$L"/.download-* 2>/dev/null
echo result=ok
`, "T", task)
}

// instance reads the pid file: the process id, its start time (field 22 of
// /proc/<pid>/stat, so a reused process id is told apart) and the instance
// id Corral gave it. alive is true when that very process still runs (and
// is not a zombie). An old pid file holds only a process id: legacy is true
// when that process runs one of Corral's installed releases.
const instance = `P="$ST/code-server.pid"
starttime() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }
running() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | awk -v st="$2" '$1 != "Z" && $20 == st {ok=1} END {exit !ok}'; }
pid=; st=; id=
[ -f "$P" ] && read -r pid st id <"$P"
alive() { [ -n "$pid" ] && [ -n "$st" ] && running "$pid" "$st"; }
legacy() { [ -n "$pid" ] && [ -z "$st" ] && [ -d "/proc/$pid" ] && tr '\000' '\n' <"/proc/$pid/cmdline" 2>/dev/null | awk -v p="$L/code-server-" 'index($0, p) == 1 {f = 1} END {exit !f}'; }
# halt ends the recorded process group and waits until it is gone.
halt() {
  kill -TERM "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null
  i=0
  while [ -d "/proc/$pid" ] && ! grep -q '^State:.*Z' "/proc/$pid/status" 2>/dev/null && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done
  kill -KILL "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null
  i=0
  while [ -d "/proc/$pid" ] && ! grep -q '^State:.*Z' "/proc/$pid/status" 2>/dev/null && [ $i -lt 5 ]; do sleep 1; i=$((i+1)); done
  ! { [ -d "/proc/$pid" ] && ! grep -q '^State:.*Z' "/proc/$pid/status" 2>/dev/null; }
}
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

// StartCommand starts code-server detached in its own session and process
// group (checked), or reports that it already runs. An earlier instance that
// runs without its socket, or one recorded in the old pid file format, is
// stopped first; a recorded process that cannot be verified is not
// overwritten. instanceID is recorded with the process id and start time.
func StartCommand(version, socket, instanceID string) string {
	return script(layout+lock+instance+`umask 077
D="$L/code-server-$V"
mkdir -p "$DATA/extensions" && chmod 700 "$HOME/.local/share/corral" "$DATA" || { echo error=mkdir; exit 1; }
`+socketDir+`if alive && [ -S "$S" ]; then echo result=running; echo "instance=$id"; exit 0; fi
if alive || legacy; then
  halt || { echo error=kill; echo "detail=an earlier code-server (pid $pid) would not stop"; exit 1; }
elif [ -n "$pid" ] && [ -z "$st" ] && [ -d "/proc/$pid" ] && [ ! -r "/proc/$pid/cmdline" ]; then
  echo error=verify; echo "detail=cannot tell whether process $pid from the pid file is code-server"; exit 1
fi
[ -x "$D/bin/code-server" ] || { echo error=notinstalled; exit 1; }
rm -f "$S"
setsid nohup "$D/bin/code-server" --socket "$S" --socket-mode 600 --auth none --disable-telemetry \
  --disable-update-check --disable-proxy --disable-getting-started-override \
  --user-data-dir "$DATA" --extensions-dir "$DATA/extensions" \
  >"$ST/code-server.log" 2>&1 </dev/null 9>&- & # (it must not keep the lock)
new=$!
echo "$new $(starttime "$new") $I" >"$P"
i=0
while [ $i -lt 30 ]; do
  if [ -S "$S" ]; then
    # In a session and process group of its own, or stopping it would not reach all of it.
    if [ "$(sed 's/^.*) //' "/proc/$new/stat" 2>/dev/null | cut -d' ' -f3)" != "$new" ]; then
      kill "$new" 2>/dev/null; rm -f "$P"; echo error=nosetsid; exit 1
    fi
    echo result=started; echo "instance=$I"; exit 0
  fi
  kill -0 "$new" 2>/dev/null || break # (its command line is in flux while it starts)
  sleep 1; i=$((i+1))
done
echo error=start
echo "detail=$(tail -n 5 "$ST/code-server.log" 2>/dev/null | tr '\n' ' ' | cut -c1-300)"
exit 1
`, "V", version, "S", socket, "I", instanceID)
}

// StopCommand stops the code-server named by the pid file (its whole
// process group), but only that very process: same id and start time, and,
// when instanceID is given, that instance (a newer one is left running and
// reported as result=other). An old-format pid file is honoured when its
// process runs one of Corral's releases. It fails, and leaves the state in
// place, when it cannot read the pid file or the process survives.
func StopCommand(socket, instanceID string) string {
	return script(layout+lock+instance+`if [ ! -f "$P" ]; then rm -f "$S"; echo result=ok; exit 0; fi
if [ -z "$pid" ]; then echo error=verify; exit 1; fi
if [ -n "$I" ] && [ -n "$id" ] && [ "$id" != "$I" ] && alive; then echo result=other; echo "instance=$id"; exit 0; fi
if alive || legacy; then
  halt || { echo error=kill; exit 1; }
fi
rm -f "$P" "$S"
echo result=ok
`, "S", socket, "I", instanceID)
}

// ValidVersion reports whether a version can be put into paths and commands.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }
