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
	if f := run(t, home, StopCommand(evil, "")); f["result"] != "ok" {
		t.Fatalf("stop: %v", f)
	}
	for _, n := range []string{"pwned", "pwned2", "pwned3"} {
		if _, err := os.Stat(filepath.Join(home, n)); err == nil {
			t.Fatalf("value ran as a command (%s)", n)
		}
	}
	for _, c := range []string{ProbeCommand("1.2.3"),
		InstallCommand(task, "1.2.3", "amd64", FromStdin, "", "h"), CancelInstallCommand(task), StartCommand("1.2.3", "/s", "i"), StopCommand("/s", "")} {
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
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Linkname: e.link, Size: int64(len(e.body))}
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
	if f := run(t, home, InstallCommand(task, "4.138.0", localArch(), FromDownload, url, strings.Repeat("0", 64)), path); f["error"] != "checksum" {
		t.Fatalf("wrong digest: %v", f)
	}
	if installed(home) {
		t.Fatal("unpacked despite a wrong digest")
	}
	noStaging(t, home)
	if f := run(t, home, InstallCommand(task, "4.138.0", localArch(), FromDownload, url, fileSHA(t, release)), path); f["result"] != "ok" ||
		f["phase"] != "downloading" {
		t.Fatalf("install: %v", f)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "--proto =https --proto-redir =https") || !strings.Contains(string(b), url) {
		t.Fatalf("download not limited to HTTPS: %s", b)
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
	if f := run(t, home2, InstallCommand(task, "4.138.0", "amd64", FromDownload, url, fileSHA(t, release)), failing); f["error"] != "download" ||
		!strings.Contains(f["detail"], "resolve") {
		t.Fatalf("failed download: %v", f)
	}
	noStaging(t, home2)
	tools := minimalPath(t, "sh", "mkdir", "chmod", "rm", "tail", "tr", "cut", "sed", "find", "flock", "setsid", "cat")
	if f := run(t, home2, InstallCommand(task, "4.138.0", "amd64", FromDownload, url, fileSHA(t, release)), tools); f["error"] != "nodownloader" {
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
	if f := runStdin(t, home, InstallCommand(task, "4.138.0", localArch(), FromStdin, "", fileSHA(t, release)), release); f["result"] != "ok" ||
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
			f := runStdin(t, home, InstallCommand(task, "4.138.0", "amd64", FromStdin, "", fileSHA(t, archive)), archive)
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
	if f := runStdin(t, home, InstallCommand(task, "4.138.0", "amd64", FromStdin, "", fileSHA(t, archive)), archive); f["error"] != "layout" || installed(home) {
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
		if f := runStdin(t, home, InstallCommand(task, "4.138.0", "amd64", FromStdin, "", fileSHA(t, release)), release, minimalPath(t, tools...)); f["error"] != want {
			t.Errorf("%s: %v", want, f)
		}
	}
	// A digest tool that fails is not taken for a digest.
	path := minimalPath(t, append([]string{"flock", "setsid"}, common...)...)
	os.WriteFile(filepath.Join(strings.TrimPrefix(path, "PATH="), "sha256sum"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if f := runStdin(t, t.TempDir(), InstallCommand(task, "4.138.0", "amd64", FromStdin, "", fileSHA(t, release)), release, path); f["error"] != "nosha" {
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
	pidFile := filepath.Join(home, ".local/state/corral/install-"+task, "task.pid")
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
	// An install that only now gets the lock gives up too.
	if f := run(t, home, CancelInstallCommand(task)); f["result"] != "ok" {
		t.Fatalf("cancel: %v", f)
	}
	if f := run(t, home, InstallCommand(task, "4.138.0", localArch(), FromDownload, "https://e/x", fileSHA(t, release)), path); f["error"] != "cancelled" {
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
	if r := run(t, home, StartCommand("4.138.0", filepath.Join(linked, "code-server.sock"), "i")); r["error"] != "socketdir" {
		t.Fatalf("symlinked socket directory: %v", r)
	}
	shared := filepath.Join(t.TempDir(), "shared")
	os.Mkdir(shared, 0o755)
	os.Chmod(shared, 0o755)
	if r := run(t, home, StartCommand("4.138.0", filepath.Join(shared, "code-server.sock"), "i")); r["error"] != "socketdir" {
		t.Fatalf("socket directory open to others: %v", r)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	r := run(t, home, StartCommand("4.138.0", filepath.Join(fresh, "code-server.sock"), "i"))
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
	if r := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "")); r["result"] != "ok" {
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
	if r := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "")); r["result"] != "ok" {
		t.Fatalf("stop: %v", r)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("own process not stopped")
	}
	// An unreadable pid file is a failure, not a success.
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte("\n"), 0o600)
	if r := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "")); r["error"] != "verify" {
		t.Fatalf("garbled pid file: %v", r)
	}
}

func TestStartDisablesBuiltInProxies(t *testing.T) {
	if c := StartCommand("4.138.0", "/s", "i"); !strings.Contains(c, "--disable-proxy") || !strings.Contains(c, "--auth none") {
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
	stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
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
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "older")); f["result"] != "other" || f["instance"] != "newer" {
		t.Fatalf("stop of an older instance: %v", f)
	}
	select {
	case <-done:
		t.Fatal("a stop for an older instance killed the newer one")
	case <-time.After(200 * time.Millisecond):
	}
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "newer")); f["result"] != "ok" || !ended(done) {
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
	old := func() chan error {
		c, done := sleeper(t, "sh", filepath.Join(bin, "code-server"))
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d\n", c.Process.Pid)), 0o600)
		return done
	}
	done := old()
	if f := run(t, home, StopCommand(filepath.Join(st, "code-server.sock"), "")); f["result"] != "ok" || !ended(done) {
		t.Fatalf("stop with an old pid file: %v", f)
	}
	done = old()
	sock := filepath.Join(st, "code-server.sock")
	// Starting (here it then finds no release) stops the old instance first.
	if f := run(t, home, StartCommand("4.138.0", sock, "fresh")); f["error"] != "notinstalled" || !ended(done) {
		t.Fatalf("start over an old instance: %v", f)
	}
	// A process that is not one of ours is left alone.
	c, other := sleeper(t, "sleep", "30")
	os.WriteFile(filepath.Join(st, "code-server.pid"), []byte(fmt.Sprintf("%d\n", c.Process.Pid)), 0o600)
	run(t, home, StopCommand(sock, ""))
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
	if f := run(t, home, StartCommand("4.138.0", filepath.Join(st, "code-server.sock"), "fresh")); f["error"] != "notinstalled" || !ended(done) {
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
