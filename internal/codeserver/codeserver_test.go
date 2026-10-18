package codeserver

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	p, err := SocketPath("/home/alice", "1000")
	if err != nil || p != "/home/alice/.local/state/corral/code-server.sock" {
		t.Fatalf("%q %v", p, err)
	}
	long := "/home/" + strings.Repeat("a", 80)
	p, err = SocketPath(long, "1000")
	if err != nil || len(p) > MaxSocketPath || !strings.HasPrefix(p, "/tmp/corral-1000-") {
		t.Fatalf("long home: %q %v", p, err)
	}
	if q, _ := SocketPath(long+"b", "1000"); q == p {
		t.Fatal("two homes share a socket directory")
	}
	for _, bad := range [][2]string{{"relative", "1000"}, {"/home/a", "10 00"}, {"/home/a", "-1"}, {"/home/a", ""}} {
		if _, err := SocketPath(bad[0], bad[1]); err == nil {
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
	if f := run(t, home, StopCommand(evil)); f["result"] != "ok" {
		t.Fatalf("stop: %v", f)
	}
	for _, n := range []string{"pwned", "pwned2", "pwned3"} {
		if _, err := os.Stat(filepath.Join(home, n)); err == nil {
			t.Fatalf("value ran as a command (%s)", n)
		}
	}
	for _, c := range []string{ProbeCommand("1.2.3"), PrepareCommand(), DownloadCommand("1.2.3", "amd64", "https://e/x"),
		ExtractCommand("1.2.3", "amd64", "a"), StartCommand("1.2.3", "/s"), StopCommand("/s")} {
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

// The download uses curl when there is one, then wget; neither is a clear error.
func TestDownloadAndExtract(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	release := filepath.Join(t.TempDir(), "release.tgz")
	writeRelease(t, release, "4.138.0", localArch(), "#!/bin/sh\necho fake\n")
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	fake := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >>" + log + "\nout=\nwhile [ $# -gt 0 ]; do case \"$1\" in -o|-O) shift; out=\"$1\";; esac; shift; done\ncp " + release + " \"$out\"\n"
	os.WriteFile(filepath.Join(bin, "curl"), []byte(fake), 0o755)
	url := "https://mirror.example/v4.138.0/code-server.tgz"
	if f := run(t, home, DownloadCommand("4.138.0", localArch(), url), "PATH="+bin+":"+os.Getenv("PATH")); f["result"] != "ok" {
		t.Fatalf("download: %v", f)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "curl -fsSL") || !strings.Contains(string(b), url) {
		t.Fatalf("curl not called with the URL: %s", b)
	}
	if f := run(t, home, ExtractCommand("4.138.0", localArch(), DownloadPath("4.138.0", localArch()))); f["result"] != "ok" {
		t.Fatalf("extract: %v", f)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/lib/corral/code-server-4.138.0/bin/code-server")); err != nil {
		t.Fatal("release not in place")
	}
	if _, err := os.Stat(filepath.Join(home, DownloadPath("4.138.0", localArch()))); err == nil {
		t.Fatal("archive left behind")
	}
	if fi, _ := os.Stat(filepath.Join(home, ".local/lib/corral")); fi == nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("library directory mode %v", fi.Mode())
	}

	// A broken archive is reported and nothing is installed.
	home2 := t.TempDir()
	os.MkdirAll(filepath.Join(home2, ".local/lib/corral"), 0o700)
	os.WriteFile(filepath.Join(home2, DownloadPath("4.138.0", "amd64")), []byte("not a tarball"), 0o600)
	if f := run(t, home2, ExtractCommand("4.138.0", "amd64", DownloadPath("4.138.0", "amd64"))); f["error"] != "extract" {
		t.Fatalf("broken archive: %v", f)
	}
	if _, err := os.Stat(filepath.Join(home2, ".local/lib/corral/code-server-4.138.0")); err == nil {
		t.Fatal("broken archive installed")
	}

	// A failing download says so; without curl and wget, that is said.
	os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\necho 'Could not resolve host' >&2\nexit 6\n"), 0o755)
	if f := run(t, home2, DownloadCommand("4.138.0", "amd64", url), "PATH="+bin+":"+os.Getenv("PATH")); f["error"] != "download" ||
		!strings.Contains(f["detail"], "resolve") {
		t.Fatalf("failed download: %v", f)
	}
	tools := t.TempDir()
	for _, tool := range []string{"sh", "mkdir", "chmod", "rm", "tail", "tr"} {
		if p, err := exec.LookPath(tool); err == nil {
			os.Symlink(p, filepath.Join(tools, tool))
		}
	}
	if f := run(t, home2, DownloadCommand("4.138.0", "amd64", url), "PATH="+tools); f["error"] != "nodownloader" {
		t.Fatalf("no downloader: %v", f)
	}
}
