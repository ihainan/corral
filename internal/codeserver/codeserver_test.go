package codeserver

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// run executes a command like the host would, with HOME=home.
func run(t *testing.T, home, cmd string, env ...string) map[string]string {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
	out, _ := c.Output()
	return Fields(out)
}

func needSh(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

func TestSocketPathStaysShort(t *testing.T) {
	p, err := SocketPath("/home/alice", "1000", "/run/user/1000")
	if err != nil || p != "/home/alice/.local/state/corral/code-server.sock" {
		t.Fatalf("%q %v", p, err)
	}
	long := "/home/" + strings.Repeat("a", 80)
	if p, err = SocketPath(long, "1000", "/run/user/1000"); err != nil || p != "/run/user/1000/corral/code-server.sock" {
		t.Fatalf("long home with a runtime directory: %q %v", p, err)
	}
	p, err = SocketPath(long, "1000", "")
	if err != nil || len(p) > MaxSocketPath || !strings.HasPrefix(p, "/tmp/corral-1000-") {
		t.Fatalf("long home: %q %v", p, err)
	}
	if q, _ := SocketPath(long+"b", "1000", ""); q == p {
		t.Fatal("two homes share a socket directory")
	}
	for _, bad := range [][2]string{{"relative", "1000"}, {"/home/a", "10 00"}, {"/home/a", "-1"}, {"/home/a", ""}} {
		if _, err := SocketPath(bad[0], bad[1], ""); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestProbeOnThisMachine(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	f := run(t, home, ProbeCommand("4.138.0"))
	p, err := ParseProbe(f)
	if _, known := Arches[f["machine"]]; !known {
		if err == nil {
			t.Fatal("unknown architecture accepted")
		}
		t.Skipf("machine %q", f["machine"])
	}
	if err != nil || p.Home != home || p.UID == "" || p.Installed || !p.Tar {
		t.Fatalf("probe: %+v %v (%v)", p, err, f)
	}
	os.MkdirAll(filepath.Join(home, ".local/lib/corral/code-server-4.138.0/bin"), 0o700)
	os.WriteFile(filepath.Join(home, ".local/lib/corral/code-server-4.138.0/bin/code-server"), []byte("#!/bin/sh\n"), 0o700)
	if p, _ := ParseProbe(run(t, home, ProbeCommand("4.138.0"))); p == nil || !p.Installed {
		t.Fatal("installed release not seen")
	}
}

func TestParseProbe(t *testing.T) {
	base := map[string]string{"result": "ok", "uid": "1000", "home": "/home/a", "machine": "aarch64", "free": "123", "tar": "1"}
	p, err := ParseProbe(base)
	if err != nil || p.Arch != "arm64" || p.FreeKB != 123 {
		t.Fatalf("%+v %v", p, err)
	}
	for k, v := range map[string]string{"machine": "riscv64", "uid": "x", "home": "relative", "result": ""} {
		f := map[string]string{}
		for a, b := range base {
			f[a] = b
		}
		f[k] = v
		if _, err := ParseProbe(f); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}

// Values cannot break out of their quotes, whatever the host reports.
func TestCommandsQuoteTheirValues(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	evil := filepath.Join(home, "x'; touch pwned; '$(touch pwned2)`touch pwned3`", "s.sock")
	if f := run(t, home, StopCommand(evil, "0123456789abcdef", op())); f["result"] != "ok" {
		t.Fatalf("stop: %v", f)
	}
	for _, n := range []string{"pwned", "pwned2", "pwned3"} {
		if _, err := os.Stat(filepath.Join(home, n)); err == nil {
			t.Fatalf("value ran as a command (%s)", n)
		}
	}
	for _, c := range []string{ProbeCommand("1.2.3"),
		InstallCommand(task, "1.2.3", "amd64", FromStdin, "", "h"), CancelInstallCommand(task), StartCommand("1.2.3", "/s", "i", task),
		StopCommand("/s", "t", task), ForceStopCommand("/s", task), SettleCommand(task), InstanceCommand()} {
		if !strings.HasPrefix(c, "sh -c '") || !strings.HasSuffix(c, " "+Tag) {
			t.Errorf("unexpected command shape: %.60s", c)
		}
	}
}

// writeRelease builds a release archive with a stand-in bin/code-server.
func writeRelease(t *testing.T, path, version, arch, script string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	root := "code-server-" + version + "-linux-" + arch + "/"
	tw.WriteHeader(&tar.Header{Name: root, Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: root + "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: root + "bin/code-server", Mode: 0o755, Size: int64(len(script))})
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
}

func localArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// entry is one member of a test archive.
type entry struct {
	name, link string
	typ        byte
	body       string
	mode       int64
}

// writeArchive builds an archive from entries.
func writeArchive(t *testing.T, path string, entries []entry) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o755
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ == tar.TypeDir || e.typ == tar.TypeSymlink || e.typ == tar.TypeLink {
			h.Size = 0
		}
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const task = "0123456789abcdef"

var opN atomic.Uint64

// op is a fresh operation id.
func op() string { return fmt.Sprintf("%016x", opN.Add(1)) }

// fakeCurl puts a curl on PATH that logs its arguments and serves release.
func fakeCurl(t *testing.T, release, extra string) (path, log string) {
	bin := t.TempDir()
	log = filepath.Join(bin, "log")
	fake := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >>" + log + "\n" + extra +
		"out=\nwhile [ $# -gt 0 ]; do case \"$1\" in -o|-O) shift; out=\"$1\";; esac; shift; done\ncp " + release + " \"$out\"\n"
	os.WriteFile(filepath.Join(bin, "curl"), []byte(fake), 0o755)
	return "PATH=" + bin + ":" + os.Getenv("PATH"), log
}

func installed(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".local/lib/corral/code-server-4.138.0/bin/code-server"))
	return err == nil
}

func noStaging(t *testing.T, home string) {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(home, ".local/state/corral/install-*")); len(m) != 0 {
		t.Fatalf("staging left behind: %v", m)
	}
}

// One install: download (HTTPS only), digest, unpack, all inside its own
// staging directory, which is gone afterwards.
func TestInstallDownloads(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	release := filepath.Join(t.TempDir(), "release.tgz")
	writeRelease(t, release, "4.138.0", localArch(), "#!/bin/sh\necho fake\n")
	path, log := fakeCurl(t, release, "")
	url := "https://mirror.example/v4.138.0/code-server.tgz"
	// A wrong digest: nothing unpacked.
	if f := run(t, home, InstallCommand(op(), "4.138.0", localArch(), FromDownload, url, strings.Repeat("0", 64)), path); f["error"] != "checksum" {
		t.Fatalf("wrong digest: %v", f)
	}
	if installed(home) {
		t.Fatal("unpacked despite a wrong digest")
	}
	noStaging(t, home)
	if f := run(t, home, InstallCommand(op(), "4.138.0", localArch(), FromDownload, url, fileSHA(t, release)), path); f["result"] != "ok" ||
		f["phase"] != "downloading" {
		t.Fatalf("install: %v", f)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "--proto =https --proto-redir =https") || !strings.Contains(string(b), url) {
		t.Fatalf("download not limited to HTTPS: %s", b)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), fmt.Sprintf("--max-time %d", DownloadSeconds)) ||
		!strings.Contains(string(b), fmt.Sprintf("--retry-max-time %d", DownloadSeconds)) {
		t.Fatalf("download not bounded in time: %s", b)
	}
	if !installed(home) {
		t.Fatal("release not in place")
	}
	noStaging(t, home)
	if fi, _ := os.Stat(filepath.Join(home, ".local/lib/corral")); fi == nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("library directory mode %v", fi.Mode())
	}

	// A failing download says so; without curl and wget, that is said.
	home2 := t.TempDir()
	failing, _ := fakeCurl(t, release, "echo 'Could not resolve host' >&2\nexit 6\n")
	if f := run(t, home2, InstallCommand(op(), "4.138.0", "amd64", FromDownload, url, fileSHA(t, release)), failing); f["error"] != "download" ||
		!strings.Contains(f["detail"], "resolve") {
		t.Fatalf("failed download: %v", f)
	}
	noStaging(t, home2)
	tools := minimalPath(t, "sh", "mkdir", "chmod", "rm", "tail", "tr", "cut", "sed", "find", "flock", "setsid", "cat", "mv", "awk")
	if f := run(t, home2, InstallCommand(op(), "4.138.0", "amd64", FromDownload, url, fileSHA(t, release)), tools); f["error"] != "nodownloader" {
		t.Fatalf("no downloader: %v", f)
	}
}

// minimalPath is a PATH with only these tools.
func minimalPath(t *testing.T, tools ...string) string {
	dir := t.TempDir()
	for _, tool := range tools {
		if p, err := exec.LookPath(tool); err == nil {
			os.Symlink(p, filepath.Join(dir, tool))
		}
	}
	return "PATH=" + dir
}

// runStdin runs a command with the file on its input.
func runStdin(t *testing.T, home, cmd, input string, env ...string) map[string]string {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
	in, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	c.Stdin = in
	out, _ := c.Output()
	return Fields(out)
}

// The gateway's own copy comes in on the command's input, under the same lock.
func TestInstallFromInput(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	release := filepath.Join(t.TempDir(), "release.tgz")
	writeRelease(t, release, "4.138.0", localArch(), "#!/bin/sh\n")
	if f := runStdin(t, home, InstallCommand(op(), "4.138.0", localArch(), FromStdin, "", fileSHA(t, release)), release); f["result"] != "ok" ||
		f["phase"] != "uploading" || !installed(home) {
		t.Fatalf("install from input: %v", f)
	}
	noStaging(t, home)
}

// Members must stay inside the release's top directory: no absolute paths,
// no "..", no links pointing out; links inside are fine.
func TestInstallChecksMembers(t *testing.T) {
	needSh(t)
	top := "code-server-4.138.0-linux-amd64/"
	base := []entry{{name: top, typ: tar.TypeDir}, {name: top + "bin/", typ: tar.TypeDir},
		{name: top + "bin/code-server", typ: tar.TypeReg, body: "#!/bin/sh\n"}}
	for name, extra := range map[string][]entry{
		"ok, link inside":   {{name: top + "lib/", typ: tar.TypeDir}, {name: top + "lib/cs", typ: tar.TypeSymlink, link: "../bin/code-server"}},
		"absolute path":     {{name: "/tmp/corral-evil", typ: tar.TypeReg, body: "x"}},
		"dot dot":           {{name: top + "../evil", typ: tar.TypeReg, body: "x"}},
		"dot dot, back in":  {{name: top + "../../a/" + top + "x", typ: tar.TypeReg, body: "x"}}, // ../a/<top>/x
		"other top":         {{name: "elsewhere/x", typ: tar.TypeReg, body: "x"}},
		"link out":          {{name: top + "out", typ: tar.TypeSymlink, link: "../../../etc/passwd"}},
		"absolute link":     {{name: top + "out", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hard link outside": {{name: top + "hard", typ: tar.TypeLink, link: "etc/passwd"}},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			archive := filepath.Join(t.TempDir(), "a.tgz")
			writeArchive(t, archive, append(append([]entry{}, base...), extra...))
			f := runStdin(t, home, InstallCommand(op(), "4.138.0", "amd64", FromStdin, "", fileSHA(t, archive)), archive)
			if strings.HasPrefix(name, "ok") {
				if f["result"] != "ok" {
					t.Fatalf("good archive refused: %v", f)
				}
				return
			}
			if f["error"] != "members" || installed(home) {
				t.Fatalf("bad archive: %v", f)
			}
			if _, err := os.Lstat("/tmp/corral-evil"); err == nil && name == "absolute path" {
				t.Fatal("member written outside")
			}
			noStaging(t, home)
		})
	}
}

// A release whose entry point is a symbolic link is not installed, even
// with the right digest.
func TestInstallRefusesLinkedEntryPoint(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	top := "code-server-4.138.0-linux-amd64/"
	archive := filepath.Join(t.TempDir(), "a.tgz")
	writeArchive(t, archive, []entry{{name: top, typ: tar.TypeDir}, {name: top + "bin/", typ: tar.TypeDir},
		{name: top + "real", typ: tar.TypeReg, body: "#!/bin/sh\n"}, {name: top + "bin/code-server", typ: tar.TypeSymlink, link: "../real"}})
	if f := runStdin(t, home, InstallCommand(op(), "4.138.0", "amd64", FromStdin, "", fileSHA(t, archive)), archive); f["error"] != "layout" || installed(home) {
		t.Fatalf("linked entry point: %v", f)
	}
}

// Without sha256sum or shasum, flock or setsid, nothing is installed.
func TestInstallNeedsItsTools(t *testing.T) {
	needSh(t)
	release := filepath.Join(t.TempDir(), "release.tgz")
	writeRelease(t, release, "4.138.0", "amd64", "#!/bin/sh\n")
	common := []string{"sh", "mkdir", "chmod", "rm", "cut", "tar", "tail", "tr", "mv", "gzip", "sed", "awk", "find", "cat"}
	for want, tools := range map[string][]string{
		"nosha":    append([]string{"flock", "setsid"}, common...),
		"noflock":  append([]string{"setsid", "sha256sum"}, common...),
		"nosetsid": append([]string{"flock", "sha256sum"}, common...),
	} {
		home := t.TempDir()
		if f := runStdin(t, home, InstallCommand(op(), "4.138.0", "amd64", FromStdin, "", fileSHA(t, release)), release, minimalPath(t, tools...)); f["error"] != want {
			t.Errorf("%s: %v", want, f)
		}
	}
	// A digest tool that fails is not taken for a digest.
	path := minimalPath(t, append([]string{"flock", "setsid"}, common...)...)
	os.WriteFile(filepath.Join(strings.TrimPrefix(path, "PATH="), "sha256sum"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if f := runStdin(t, t.TempDir(), InstallCommand(op(), "4.138.0", "amd64", FromStdin, "", fileSHA(t, release)), release, path); f["error"] != "nosha" {
		t.Errorf("failing sha256sum: %v", f)
	}
}

// Cancelling an install ends its whole process group (here: a download that
// would take a minute), waits for it, and removes its staging directory.
func TestCancelEndsTheInstall(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	release := filepath.Join(t.TempDir(), "release.tgz")
	writeRelease(t, release, "4.138.0", localArch(), "#!/bin/sh\n")
	path, _ := fakeCurl(t, release, "sleep 60\n")
	c := exec.Command("sh", "-c", InstallCommand(task, "4.138.0", localArch(), FromDownload, "https://e/x", fileSHA(t, release)))
	c.Env = []string{"HOME=" + home, path}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	pidFile := filepath.Join(home, ".local/state/corral/op-"+task, "pid")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("install never started")
		}
	}
	b, _ := os.ReadFile(pidFile)
	group := strings.Fields(string(b))[0]
	if f := run(t, home, CancelInstallCommand(task)); f["result"] != "ok" {
		t.Fatalf("cancel: %v", f)
	}
	if pgid, _ := strconv.Atoi(group); pgid <= 0 || syscall.Kill(-pgid, 0) == nil {
		t.Fatal("install's process group survived the cancel")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("install script still running")
	}
	if installed(home) {
		t.Fatal("installed despite the cancel")
	}
	noStaging(t, home)
	// An install cancelled before it ran never runs.
	later := op()
	if f := run(t, home, CancelInstallCommand(later)); f["result"] != "ok" {
		t.Fatalf("cancel: %v", f)
	}
	if f := run(t, home, InstallCommand(later, "4.138.0", localArch(), FromDownload, "https://e/x", fileSHA(t, release)), path); f["error"] != "cancelled" {
		t.Fatalf("install after its cancel: %v", f)
	}
}

// The socket directory must be a real, private directory of the account:
// a symbolic link (also to a directory of the account) or a shared directory
// is refused, and nothing is created through them.
func TestStartRefusesUnsafeSocketDirectory(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	victim := t.TempDir()
	os.Chmod(victim, 0o700)
	linked := filepath.Join(t.TempDir(), "corral-sock")
	os.Symlink(victim, linked)
	if r := run(t, home, StartCommand("4.138.0", filepath.Join(linked, "code-server.sock"), "i", op())); r["error"] != "socketdir" {
		t.Fatalf("symlinked socket directory: %v", r)
	}
	shared := filepath.Join(t.TempDir(), "shared")
	os.Mkdir(shared, 0o755)
	os.Chmod(shared, 0o755)
	if r := run(t, home, StartCommand("4.138.0", filepath.Join(shared, "code-server.sock"), "i", op())); r["error"] != "socketdir" {
		t.Fatalf("socket directory open to others: %v", r)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	r := run(t, home, StartCommand("4.138.0", filepath.Join(fresh, "code-server.sock"), "i", op()))
	if r["error"] != "notinstalled" { // past the directory check
		t.Fatalf("fresh socket directory: %v", r)
	}
	if fi, err := os.Lstat(fresh); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory created as %v %v", fi.Mode(), err)
	}
}

// Stop kills only the very process it started: a reused process id (same
// number, different start time) is left alone.
func TestStopKillsOnlyItsOwnProcess(t *testing.T) {
	needSh(t)
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	os.MkdirAll(st, 0o700)
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer bystander.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- bystander.Wait() }() // reaped as soon as it ends, like a daemon under init
	pid := bystander.Process.Pid
	// The pid file names this pid, but with another start time.
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d 1 old\n", pid)), 0o600)
	if r := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "old", op())); r["result"] != "ok" {
		t.Fatalf("stop: %v", r)
	}
	select {
	case <-done:
		t.Fatal("stop killed a process that only shared the pid")
	case <-time.After(200 * time.Millisecond):
	}
	// With the right start time it is stopped.
	stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+2:])
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d %s cur\n", pid, fields[19])), 0o600)
	if r := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "cur", op())); r["result"] != "ok" {
		t.Fatalf("stop: %v", r)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("own process not stopped")
	}
	// An unreadable pid file is a failure, not a success.
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte("\n"), 0o600)
	if r := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "cur", op())); r["error"] != "verify" {
		t.Fatalf("garbled pid file: %v", r)
	}
}

func TestStartDisablesBuiltInProxies(t *testing.T) {
	if c := StartCommand("4.138.0", "/s", "i", op()); !strings.Contains(c, "--disable-proxy") || !strings.Contains(c, "--auth none") {
		t.Fatal("code-server started with its own port proxy")
	}
}

// sleeper starts a process in its own session whose command line is c, and
// reaps it when it ends (like init would).
func sleeper(t *testing.T, name string, args ...string) (*exec.Cmd, chan error) {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	return c, done
}

func startTime(t *testing.T, pid int) string {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "" // ended
	}
	return strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+2:])[19]
}

func ended(done chan error) bool {
	select {
	case <-done:
		return true
	case <-time.After(15 * time.Second):
		return false
	}
}

// A stop for one instance leaves a newer one alone.
func TestStopLeavesANewerInstance(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	os.MkdirAll(st, 0o700)
	c, done := sleeper(t, "sleep", "30")
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d %s newer\n", c.Process.Pid, startTime(t, c.Process.Pid))), 0o600)
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "older", op())); f["result"] != "other" || f["instance"] != "newer" {
		t.Fatalf("stop of an older instance: %v", f)
	}
	select {
	case <-done:
		t.Fatal("a stop for an older instance killed the newer one")
	case <-time.After(200 * time.Millisecond):
	}
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "newer", op())); f["result"] != "ok" || !ended(done) {
		t.Fatalf("stop of the instance itself: %v", f)
	}
}

// An instance recorded in the old pid file format (just a process id) is
// recognised by its command line and stopped, before a start too.
func TestOldPidFileIsHonoured(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	bin := filepath.Join(home, ".local/lib/corral/code-server-4.137.0/bin")
	os.MkdirAll(st, 0o700)
	os.MkdirAll(bin, 0o700)
	os.WriteFile(filepath.Join(bin, "code-server"), []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	old := func() (string, chan error) {
		c, done := sleeper(t, "sh", filepath.Join(bin, "code-server"))
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d\n", c.Process.Pid)), 0o600)
		return fmt.Sprintf("pid:%d", c.Process.Pid), done
	}
	target, done := old()
	if f := run(t, home, InstanceCommand()); f["instance"] != target {
		t.Fatalf("old instance named %v, want %s", f, target)
	}
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "pid:1", op())); f["result"] != "other" || f["instance"] != target {
		t.Fatalf("stop of another old instance: %v", f)
	}
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), target, op())); f["result"] != "ok" || !ended(done) {
		t.Fatalf("stop with an old pid file: %v", f)
	}
	_, done = old()
	sock := filepath.Join(st, "code-server.sock")
	// Starting (here it then finds no release) stops the old instance first.
	if f := run(t, home, StartCommand("4.138.0", sock, "fresh", op())); f["error"] != "notinstalled" || !ended(done) {
		t.Fatalf("start over an old instance: %v", f)
	}
	// A process that is not one of ours is left alone.
	c, other := sleeper(t, "sleep", "30")
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d\n", c.Process.Pid)), 0o600)
	run(t, home, StopCommand(sock, fmt.Sprintf("pid:%d", c.Process.Pid), op()))
	select {
	case <-other:
		t.Fatal("stopped a process that only had the recorded pid")
	case <-time.After(200 * time.Millisecond):
	}
}

// A running instance whose socket is gone is stopped before starting anew.
func TestInstanceWithoutSocketIsReplaced(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	os.MkdirAll(st, 0o700)
	c, done := sleeper(t, "sleep", "30")
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d %s lost\n", c.Process.Pid, startTime(t, c.Process.Pid))), 0o600)
	if f := run(t, home, StartCommand("4.138.0", filepath.Join(st, "code-server.sock"), "fresh", op())); f["error"] != "notinstalled" || !ended(done) {
		t.Fatalf("instance without socket: %v", f)
	}
}

// $XDG_RUNTIME_DIR is only offered when it is a private directory of the account.
func TestProbeChecksTheRuntimeDirectory(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	good := t.TempDir()
	os.Chmod(good, 0o700)
	if f := run(t, home, ProbeCommand("4.138.0"), "XDG_RUNTIME_DIR="+good); f["runtime"] != good {
		t.Fatalf("private runtime directory not offered: %v", f)
	}
	shared := t.TempDir()
	os.Chmod(shared, 0o755)
	link := filepath.Join(t.TempDir(), "rt")
	os.Symlink(good, link)
	for _, bad := range []string{shared, link, "/nonexistent"} {
		if f := run(t, home, ProbeCommand("4.138.0"), "XDG_RUNTIME_DIR="+bad); f["runtime"] != "" {
			t.Errorf("%s offered as runtime directory", bad)
		}
	}
}

// groupAlive reports whether process group g has a live (not zombie) member.
func groupAlive(g int) bool {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, p := range stats {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		f := strings.Fields(string(b)[strings.LastIndex(string(b), ")")+2:])
		if len(f) > 2 && f[0] != "Z" && f[2] == strconv.Itoa(g) {
			return true
		}
	}
	return false
}

// holdLock holds the host lock from another process until the returned
// function is called.
func holdLock(t *testing.T, home string) func() {
	st := filepath.Join(home, ".local/state/corral")
	os.MkdirAll(st, 0o700)
	c := exec.Command("flock", filepath.Join(st, ".lock"), "sleep", "60")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Skip("needs flock")
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	time.Sleep(200 * time.Millisecond)
	var once sync.Once
	release := func() {
		once.Do(func() {
			syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			<-done
		})
	}
	t.Cleanup(release)
	return release
}

// leftGroup starts a process group whose leader ends at once, leaving a
// member behind (as an install's download or code-server's helpers may), and
// returns the leader's id and start time.
func leftGroup(t *testing.T) (int, string) {
	c := exec.Command("sh", "-c", "sleep 30 & sleep 0.5")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	time.Sleep(100 * time.Millisecond)
	st := startTime(t, pid)
	c.Wait()
	if !groupAlive(pid) {
		t.Fatal("no member left in the group")
	}
	return pid, st
}

// A cancel that comes while the install still waits for the host's lock
// settles it: the install never runs.
func TestCancelBeforeTheInstallRuns(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	release := filepath.Join(t.TempDir(), "release.tgz")
	writeRelease(t, release, "4.138.0", localArch(), "#!/bin/sh\n")
	path, log := fakeCurl(t, release, "")
	unlock := holdLock(t, home)
	id := op()
	res := make(chan map[string]string, 1)
	go func() {
		res <- run(t, home, InstallCommand(id, "4.138.0", localArch(), FromDownload, "https://e/x", fileSHA(t, release)), path)
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if f := run(t, home, CancelInstallCommand(id)); f["result"] != "ok" || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel of a waiting install: %v (%v)", f, time.Since(start))
	}
	unlock()
	if f := <-res; f["error"] != "cancelled" {
		t.Fatalf("install after its cancel: %v", f)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 || installed(home) {
		t.Fatal("a cancelled install ran")
	}
}

// "Gone" means the whole process group, not just its leader.
func TestCancelEndsWhatIsLeftOfTheGroup(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	id := op()
	ctl := filepath.Join(home, ".local/state/corral/op-"+id)
	os.MkdirAll(ctl, 0o700)
	pid, st := leftGroup(t)
	os.WriteFile(filepath.Join(ctl, "pid"), []byte(fmt.Sprintf("%d %s\n", pid, st)), 0o600)
	if f := run(t, home, CancelInstallCommand(id)); f["result"] != "ok" {
		t.Fatalf("cancel: %v", f)
	}
	if groupAlive(pid) {
		t.Fatal("the cancel left the rest of the install's group running")
	}
	if _, err := os.Stat(ctl); err != nil {
		t.Fatal("the control directory was removed")
	}
}

// A cancel answers ok only with proof: the group gone, the install ended, or
// nothing holding the lock any more. Otherwise it is still pending.
func TestCancelNeedsProof(t *testing.T) {
	needSh(t)
	CancelWaitSeconds = 1
	t.Cleanup(func() { CancelWaitSeconds = 20 })
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	id := op()
	os.MkdirAll(filepath.Join(st, "op-"+id), 0o700) // claimed, but no group published
	unlock := holdLock(t, home)
	if f := run(t, home, CancelInstallCommand(id)); f["error"] != "pending" {
		t.Fatalf("cancel without proof: %v", f)
	}
	os.WriteFile(filepath.Join(st, "op-"+id, "done"), nil, 0o600)
	if f := run(t, home, CancelInstallCommand(id)); f["result"] != "ok" {
		t.Fatalf("cancel of an install that ended: %v", f)
	}
	unlock()
	other := op()
	os.MkdirAll(filepath.Join(st, "op-"+other), 0o700)
	if f := run(t, home, CancelInstallCommand(other)); f["result"] != "ok" {
		t.Fatalf("cancel with nothing holding the lock: %v", f)
	}
}

// Stopping waits for the whole group: a member left after the leader ended
// is stopped too.
func TestStopEndsWhatIsLeftOfTheGroup(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	os.MkdirAll(st, 0o700)
	pid, start := leftGroup(t)
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d %s inst\n", pid, start)), 0o600)
	if f := run(t, home, InstanceCommand()); f["instance"] != "inst" {
		t.Fatalf("instance with its leader gone: %v", f)
	}
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "inst", op())); f["result"] != "ok" {
		t.Fatalf("stop: %v", f)
	}
	if groupAlive(pid) {
		t.Fatal("the stop left the rest of the group running")
	}
}

// An old instance without a process group of its own (here: in the test's
// group) is stopped by its process tree, and nothing else in its group.
func TestOldInstanceWithoutItsOwnGroup(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	bin := filepath.Join(home, ".local/lib/corral/code-server-4.137.0/bin")
	os.MkdirAll(st, 0o700)
	os.MkdirAll(bin, 0o700)
	os.WriteFile(filepath.Join(bin, "code-server"), []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755)
	c := exec.Command("sh", filepath.Join(bin, "code-server"))
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	time.Sleep(200 * time.Millisecond)
	kids, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", c.Process.Pid, c.Process.Pid))
	child, _ := strconv.Atoi(strings.TrimSpace(string(kids)))
	if child <= 0 {
		t.Fatal("no child")
	}
	t.Cleanup(func() { syscall.Kill(child, syscall.SIGKILL) })
	childStart := startTime(t, child)
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d\n", c.Process.Pid)), 0o600)
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), fmt.Sprintf("pid:%d", c.Process.Pid), op())); f["result"] != "ok" || !ended(done) {
		t.Fatalf("stop of an old instance without its own group: %v", f)
	}
	for deadline := time.Now().Add(5 * time.Second); startTime(t, child) == childStart; time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", child)); strings.Contains(string(b), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the old instance's child survived")
		}
	}
}

// What was unpacked is checked too: a link whose name holds " -> " (which the
// listing cannot tell apart), pipes and set-id files are refused; a dangling
// link inside the release is fine.
func TestInstallChecksWhatWasUnpacked(t *testing.T) {
	needSh(t)
	top := "code-server-4.138.0-linux-amd64/"
	base := []entry{{name: top, typ: tar.TypeDir}, {name: top + "bin/", typ: tar.TypeDir},
		{name: top + "bin/code-server", typ: tar.TypeReg, body: "#!/bin/sh\n"}}
	for name, extra := range map[string][]entry{
		"ok, dangling link inside": {{name: top + "lib/", typ: tar.TypeDir}, {name: top + "lib/x", typ: tar.TypeSymlink, link: "../nothing"}},
		"arrow in a link's name":   {{name: top + "y -> .", typ: tar.TypeSymlink, link: "/etc"}},
		"pipe":                     {{name: top + "fifo", typ: tar.TypeFifo}},
		"set-id file":              {{name: top + "suid", typ: tar.TypeReg, body: "x", mode: 0o4755}},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			archive := filepath.Join(t.TempDir(), "a.tgz")
			writeArchive(t, archive, append(append([]entry{}, base...), extra...))
			f := runStdin(t, home, InstallCommand(op(), "4.138.0", "amd64", FromStdin, "", fileSHA(t, archive)), archive)
			if strings.HasPrefix(name, "ok") {
				if f["result"] != "ok" {
					t.Fatalf("good archive refused: %v", f)
				}
				return
			}
			if name == "set-id file" && f["result"] == "ok" {
				// tar may drop the bits itself; then nothing set-id is installed.
				fi, err := os.Stat(filepath.Join(home, ".local/lib/corral/code-server-4.138.0/suid"))
				if err != nil || fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
					t.Fatalf("set-id file installed: %v %v", fi.Mode(), err)
				}
				return
			}
			if f["error"] != "members" || installed(home) {
				t.Fatalf("bad archive: %v", f)
			}
			noStaging(t, home)
		})
	}
}

// A stop always names its instance.
func TestStopNeedsATarget(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	st := filepath.Join(home, ".local/state/corral")
	os.MkdirAll(st, 0o700)
	c, done := sleeper(t, "sleep", "30")
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d %s a\n", c.Process.Pid, startTime(t, c.Process.Pid))), 0o600)
	sock := filepath.Join(st, "code-server.sock")
	if f := run(t, home, StopCommand(sock, "", op())); f["error"] != "target" {
		t.Fatalf("stop without a target: %v", f)
	}
	select {
	case <-done:
		t.Fatal("a stop without a target stopped something")
	case <-time.After(200 * time.Millisecond):
	}
	// Only the explicit force path stops whatever runs.
	if f := run(t, home, ForceStopCommand(sock, op())); f["result"] != "ok" || !ended(done) {
		t.Fatalf("force stop: %v", f)
	}
	if f := run(t, home, InstanceCommand()); f["result"] != "ok" || f["instance"] != "" {
		t.Fatalf("instance after the stop: %v", f)
	}
}

// An operation settled before it ran never runs.
func TestSettledOperationsNeverRun(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	id := op()
	if f := run(t, home, SettleCommand(id)); f["result"] != "ok" {
		t.Fatalf("settle: %v", f)
	}
	if f := run(t, home, StartCommand("4.138.0", filepath.Join(home, ".local/state/corral/code-server.sock"), "i", id)); f["error"] != "cancelled" {
		t.Fatalf("start after its settle: %v", f)
	}
	if f := run(t, home, StopCommand(filepath.Join(home, ".local/state/corral/code-server.sock"), "i", id)); f["error"] != "cancelled" {
		t.Fatalf("stop after its settle: %v", f)
	}
}
