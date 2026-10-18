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

// claim, right after lock, makes an operation (install, start, stop) run at
// most once, and only if nothing settled it first: it creates the
// operation's control directory op-<id> (an atomic step). If a cancel or a
// settle created it first, the operation must not run. Control directories
// stay as the record of what ran; old ones are pruned.
const claim = `find "$ST" -maxdepth 1 -name 'op-*' -mtime +1 -exec rm -rf {} + 2>/dev/null
mkdir "$ST/op-$T" 2>/dev/null || { echo error=cancelled; exit 1; }
`

// procs knows processes and process groups by /proc. A process is named by
// its id and start time (field 22 of /proc/<pid>/stat), so a reused id is
// told apart. A group is gone when no live (non-zombie) process is in it;
// while any is, no new process can be given its id.
const procs = `starttime() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }
pgrp() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f3; }
running() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | awk -v st="$2" '$1 != "Z" && $20 == st {ok=1} END {exit !ok}'; }
members() { cat /proc/[0-9]*/stat 2>/dev/null | sed 's/^.*) //' | awk -v g="$1" '$1 != "Z" && $3 == g {f = 1} END {exit !f}'; }
# ours: the group led by process $1 (started at $2) still has live members:
# the leader itself or, once it has ended, what is left of its group.
ours() { s=$(starttime "$1"); if [ -n "$s" ] && [ "$s" != "$2" ]; then return 1; fi; members "$1"; }
# ghalt ends that group (TERM, then KILL) and waits until all of it is gone.
ghalt() {
  ours "$1" "$2" || return 0
  kill -TERM "-$1" 2>/dev/null
  i=0; while ours "$1" "$2" && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done
  ours "$1" "$2" || return 0
  kill -KILL "-$1" 2>/dev/null
  i=0; while ours "$1" "$2" && [ $i -lt 5 ]; do sleep 1; i=$((i+1)); done
  ! ours "$1" "$2"
}
# tree lists process $1 and its descendants.
tree() { cat /proc/[0-9]*/stat 2>/dev/null | sed -n 's/^\([0-9][0-9]*\) .*) [A-Za-z] \([0-9][0-9]*\) .*$/\1 \2/p' |
  awk -v r="$1" '{par[$1] = $2} END {n = 1; q[1] = r; print r; for (i = 1; i <= n; i++) for (c in par) if (par[c] == q[i]) {q[++n] = c; print c}}'; }
anyleft() { for e in "$@"; do running "${e%%:*}" "${e#*:}" && return 0; done; return 1; }
# thalt ends process $1 and its descendants as they are now (each named by
# id and start time) and waits until all of them are gone.
thalt() {
  set -- $(tree "$1" | while read -r p; do s=$(starttime "$p"); [ -n "$s" ] && echo "$p:$s"; done)
  for sig in TERM KILL; do
    for e in "$@"; do running "${e%%:*}" "${e#*:}" && kill "-$sig" "${e%%:*}" 2>/dev/null; done
    i=0; while anyleft "$@" && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done
  done
  ! anyleft "$@"
}
`

// Source of the release archive for InstallCommand.
const (
	FromDownload = "download" // the host downloads it from the URL
	FromStdin    = "stdin"    // the gateway sends it on the command's input
)

// DownloadSeconds bounds a download on the host, retries included.
var DownloadSeconds = 600

// CancelWaitSeconds bounds how long a cancel waits for an install that has
// claimed its operation to show its process group (or that it ended).
var CancelWaitSeconds = 20

// installWork runs in its own session and process group (setsid), so a
// cancelled install can be ended as a whole (CancelInstallCommand). Unless
// it is cancelled already, it publishes its process id and start time in
// the control directory, then gets the archive into the staging directory,
// checks the digest, checks every listed member's path and link target stays
// inside the release's top directory, unpacks without taking owners from the
// archive, checks what was unpacked (entry point a regular executable, no
// special or set-id files, every symbolic link resolving inside the
// release), and renames the release into place. Before each step it looks
// for a cancel. It reports its phase as it goes.
const installWork = `export LC_ALL=C
me=$(sed 's/^.*) //' /proc/$$/stat 2>/dev/null)
[ "$(echo "$me" | cut -d' ' -f3)" = "$$" ] || { echo error=nosetsid; exit 1; }
cancelled() { [ -e "$C/cancel" ]; }
cancelled && { echo error=cancelled; exit 1; }
echo "$$ $(echo "$me" | cut -d' ' -f20)" >"$C/pid.new" && mv "$C/pid.new" "$C/pid" || { echo error=stage; exit 1; }
trap 'rm -rf "$STAGE"' EXIT
trap 'exit 1' HUP INT TERM PIPE
cancelled && { echo error=cancelled; exit 1; }
F="$STAGE/archive.tar.gz"
if [ "$M" = stdin ]; then
  echo phase=uploading
  cat >"$F" || { echo error=upload; exit 1; }
else
  echo phase=downloading
  # HTTPS only, redirects included; the whole download is bounded.
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 20 --max-time "$MAXT" --retry 2 --retry-max-time "$MAXT" \
      -o "$F" "$U" 2>"$STAGE/err"; rc=$?
  elif command -v wget >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
    timeout "$MAXT" wget -q --https-only -T 20 -t 2 -O "$F" "$U" 2>"$STAGE/err"; rc=$?
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
cancelled && { echo error=cancelled; exit 1; }
mkdir -m 0700 "$STAGE/x" && tar --no-same-owner -xzf "$F" -C "$STAGE/x" 2>"$STAGE/err" ||
  { echo error=extract; echo "detail=$(tail -c 300 "$STAGE/err" 2>/dev/null | tr '\n' ' ')"; exit 1; }
S="$STAGE/x/$TOP"
E="$S/bin/code-server"
if [ -L "$S" ] || [ ! -d "$S" ] || [ -L "$S/bin" ] || [ ! -d "$S/bin" ] || [ -L "$E" ] || [ ! -f "$E" ] || [ ! -x "$E" ]; then
  echo error=layout; exit 1
fi
[ -z "$(find "$STAGE/x" \( -type b -o -type c -o -type p -o -type s \) -print | head -n 1)" ] ||
  { echo error=members; echo "detail=a device file, pipe or socket"; exit 1; }
[ -z "$(find "$STAGE/x" -type f \( -perm -4000 -o -perm -2000 \) -print | head -n 1)" ] ||
  { echo error=members; echo "detail=a set-id file"; exit 1; }
R=$(cd "$S" && pwd -P) || { echo error=layout; exit 1; }
find "$STAGE/x" -type l -print >"$STAGE/links" || { echo error=members; exit 1; }
while IFS= read -r l; do
  r=$(readlink -f "$l") || { echo error=members; echo "detail=a link that does not resolve"; exit 1; }
  case "$r" in "$R"|"$R"/*) ;; *) echo error=members; echo "detail=a link that leads out of the release"; exit 1 ;; esac
done <"$STAGE/links"
cancelled && { echo error=cancelled; exit 1; }
[ -e "$D" ] || mv "$S" "$D"
[ -x "$D/bin/code-server" ] || { echo error=install; exit 1; }
echo result=ok
`

// InstallCommand installs a release on the host in one go, holding the
// host's lock throughout: everything happens in the task's own staging
// directory (created afresh, mode 0700, removed when the install has ended),
// so neither a concurrent install nor anything else can change the archive
// between its digest check and unpacking. With FromStdin the archive is read
// from the command's input. It answers only once the install's process group
// is gone. task must satisfy ValidTask; it is also the operation's id.
func InstallCommand(task, version, arch, from, url, sha256 string) string {
	return script(layout+lock+claim+procs+`umask 077
D="$L/code-server-$V"
STAGE="$ST/install-$T"
C="$ST/op-$T"
if [ -x "$D/bin/code-server" ]; then [ "$M" = stdin ] && cat >/dev/null; : >"$C/done"; echo result=ok; exit 0; fi
mkdir -p "$L" && chmod 700 "$L" || { echo error=mkdir; exit 1; }
# The lock is ours, so no install runs: staging left here is from one that was killed.
rm -rf "$ST"/install-*
mkdir -m 0700 "$STAGE" || { echo error=stage; exit 1; }
export L ST D STAGE C T V A M U H MAXT
# (In a subshell, which is no process group leader, setsid need not fork.)
( setsid sh -c "$W" corral-code-install )
rc=$?
# Answer once all of the install's group is gone.
if read -r wp wst <"$C/pid" 2>/dev/null; then while ours "$wp" "$wst"; do sleep 1; done; fi
rm -rf "$STAGE"
: >"$C/done"
exit $rc
`, "T", task, "V", version, "A", arch, "M", from, "U", url, "H", sha256, "MAXT", strconv.Itoa(DownloadSeconds), "W", installWork)
}

// CancelInstallCommand ends an install for good. If the install has not
// claimed its control directory yet, the cancel claims it, and the install
// never runs. Otherwise it marks the install cancelled (the install looks
// before each step), waits (bounded) until the install's worker has
// published its process group or is known never to run (the install ended,
// or nothing holds the host's lock any more), ends that group and waits
// until all of it is gone, then removes the staging directory. It answers
// error=kill when the group survives and error=pending when the install
// could not be told to have ended; the control directory stays either way.
func CancelInstallCommand(task string) string {
	return script(layout+procs+`STAGE="$ST/install-$T"
C="$ST/op-$T"
mkdir -p "$ST" || { echo error=mkdir; exit 1; }
if mkdir "$C" 2>/dev/null; then : >"$C/cancel"; echo result=ok; exit 0; fi
: >"$C/cancel"
free=
exec 8>>"$ST/.lock"
i=0
while [ ! -f "$C/pid" ] && [ ! -e "$C/done" ] && [ $i -lt "$WAIT" ]; do
  if command -v flock >/dev/null 2>&1 && flock -n 8; then free=1; flock -u 8; break; fi
  sleep 1; i=$((i+1))
done
exec 8>&-
if [ -f "$C/pid" ]; then
  read -r pid st <"$C/pid"
  ghalt "$pid" "$st" || { echo error=kill; exit 1; }
elif [ ! -e "$C/done" ] && [ -z "$free" ]; then
  echo error=pending; exit 1
fi
rm -rf "$STAGE"
rm -f "$L"/.upload-* "$L"/.download-* 2>/dev/null
echo result=ok
`, "T", task, "WAIT", strconv.Itoa(CancelWaitSeconds))
}

// SettleCommand makes sure an operation (a start or stop whose answer was
// lost) is over: holding the host's lock, nothing else runs; claiming the
// operation's control directory makes sure it never runs later.
func SettleCommand(op string) string {
	return script(layout+lock+`mkdir "$ST/op-$T" 2>/dev/null
echo result=ok
`, "T", op)
}

// instance reads the pid file: the process id, its start time and the
// instance id Corral gave it. An old pid file holds only a process id: legacy
// is true when that process runs one of Corral's installed releases. cur
// names the recorded instance: its id, or pid:<n> for an old one.
const instance = `P="$ST/code-server.pid"
pid=; st=; id=
[ -f "$P" ] && read -r pid st id <"$P"
# alive: the recorded instance still runs (the process, or what is left of its
# group once it has ended); up: the process runs and serves its socket.
alive() { [ -n "$pid" ] && [ -n "$st" ] && { running "$pid" "$st" || ours "$pid" "$st"; }; }
up() { [ -n "$pid" ] && [ -n "$st" ] && running "$pid" "$st" && [ -S "$S" ]; }
legacy() { [ -n "$pid" ] && [ -z "$st" ] && [ -d "/proc/$pid" ] && tr '\000' '\n' <"/proc/$pid/cmdline" 2>/dev/null | awk -v p="$L/code-server-" 'index($0, p) == 1 {f = 1} END {exit !f}'; }
unverifiable() { [ -n "$pid" ] && [ -z "$st" ] && [ -d "/proc/$pid" ] && [ ! -r "/proc/$pid/cmdline" ]; }
cur=
if [ -n "$st" ]; then cur=$id; elif [ -n "$pid" ]; then cur="pid:$pid"; fi
# halt ends the instance: its process group or, for an old instance that has
# no group of its own, its process tree; it waits until all of it is gone.
halt() {
  [ -n "$st" ] || st=$(starttime "$pid")
  if running "$pid" "$st" && [ "$(pgrp "$pid")" != "$pid" ]; then thalt "$pid"; else ghalt "$pid" "$st"; fi
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
// group (checked) as instance instanceID, or reports the instance that
// already runs. An earlier instance that runs without its socket, or one
// recorded in the old pid file format, is stopped first; a recorded process
// that cannot be verified is not overwritten. op is the operation's id (see
// SettleCommand).
func StartCommand(version, socket, instanceID, op string) string {
	return script(layout+lock+claim+procs+instance+`umask 077
D="$L/code-server-$V"
mkdir -p "$DATA/extensions" && chmod 700 "$HOME/.local/share/corral" "$DATA" || { echo error=mkdir; exit 1; }
`+socketDir+`if up; then echo result=running; echo "instance=$id"; exit 0; fi
if alive || legacy; then
  halt || { echo error=kill; echo "detail=an earlier code-server (pid $pid) would not stop"; exit 1; }
elif unverifiable; then
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
    if [ "$(pgrp "$new")" != "$new" ]; then
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
`, "V", version, "S", socket, "I", instanceID, "T", op)
}

const stopBody = `if [ ! -f "$P" ]; then rm -f "$S"; echo result=ok; exit 0; fi
if [ -z "$pid" ]; then echo error=verify; exit 1; fi
if [ "$FORCE" != 1 ] && [ "$cur" != "$I" ]; then echo result=other; echo "instance=$cur"; exit 0; fi
if alive || legacy; then
  halt || { echo error=kill; exit 1; }
elif unverifiable; then
  echo error=verify; exit 1
fi
rm -f "$P" "$S"
echo result=ok
`

// StopCommand stops instance target (an instance id, or pid:<n> for one
// recorded in the old pid file format, as InstanceCommand names it): its
// whole process group (or, for an old instance without one, its process
// tree), waiting until all of it is gone. Another instance is left running
// and reported as result=other. It fails, and leaves the state in place,
// when it cannot verify the recorded process or the process survives. op is
// the operation's id (see SettleCommand).
func StopCommand(socket, target, op string) string {
	return script(layout+lock+claim+procs+instance+`[ -n "$I" ] || { echo error=target; exit 1; }
`+stopBody, "S", socket, "I", target, "T", op, "FORCE", "0")
}

// ForceStopCommand stops whichever instance runs. Only for a host that is
// being deleted, where nothing else is to be kept running.
func ForceStopCommand(socket, op string) string {
	return script(layout+lock+claim+procs+instance+stopBody, "S", socket, "I", "", "T", op, "FORCE", "1")
}

// InstanceCommand names the instance that runs (instance=<id>, or
// instance=pid:<n> for an old one), if any.
func InstanceCommand() string {
	return script(layout + lock + procs + instance + `if alive; then echo "instance=$id"
elif legacy; then echo "instance=pid:$pid"
elif unverifiable; then echo error=verify; exit 1
fi
echo result=ok
`)
}

// ValidTarget reports whether a stop target is an instance id or pid:<n>.
func ValidTarget(t string) bool {
	if n, ok := strings.CutPrefix(t, "pid:"); ok {
		return n != "" && len(n) <= 10 && strings.Trim(n, "0123456789") == ""
	}
	return ValidTask(t)
}

// ValidVersion reports whether a version can be put into paths and commands.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }
