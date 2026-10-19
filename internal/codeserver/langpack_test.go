package codeserver

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCodeServer installs a stand-in code-server 4.138.0 under home that logs
// its arguments, "installs" an extension by making its folder (with a file
// "ok" its node looks for when checking the pack), and when run
// as a server listens on its --socket (with python3) for a few seconds.
func fakeCodeServer(t *testing.T, home string) (log string) {
	t.Helper()
	bin := filepath.Join(home, ".local/lib/corral/code-server-4.138.0/bin")
	os.MkdirAll(bin, 0o700)
	log = filepath.Join(t.TempDir(), "args")
	script := `#!/bin/sh
echo "$*" >>` + log + `
ext= sock= inst=
while [ $# -gt 0 ]; do
  case "$1" in --extensions-dir) shift; ext=$1;; --socket) shift; sock=$1;; --install-extension) shift; inst=$1;; esac
  shift
done
if [ -n "$inst" ] && [ -n "$FAKE_HANG" ]; then sleep 60 & echo $! >"$FAKE_HANG"; sleep 60; fi
if [ -n "$inst" ]; then mkdir -p "$ext/` + LangPackID + `-` + LangPackVersion + `" && touch "$ext/` + LangPackID + `-` + LangPackVersion + `/ok"; exit 0; fi
[ -n "$sock" ] && exec python3 -c 'import socket,sys,time; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); time.sleep(5)' "$sock"
`
	os.WriteFile(filepath.Join(bin, "code-server"), []byte(script), 0o700)
	// Its node checks and registers language packs: here it notes that it
	// was asked to (arguments: -e script data extensions id version locale [check]).
	os.MkdirAll(filepath.Join(bin, "..", "lib"), 0o700)
	os.WriteFile(filepath.Join(bin, "..", "lib", "node"), []byte("#!/bin/sh\necho \"node $*\" >>"+log+
		"\nif [ \"$8\" = check ]; then [ -f \"$4/$5-$6/ok\" ]; exit; fi\nexit ${FAKE_NODE_EXIT:-0}\n"), 0o700)
	return log
}

func runIn(t *testing.T, home, cmd, stdin string, env ...string) map[string]string {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
	c.Stdin = strings.NewReader(stdin)
	out, _ := c.Output()
	return Fields(out)
}

func withPackSum(t *testing.T, body string) {
	sum := sha256.Sum256([]byte(body))
	old := langPackSum
	langPackSum = hex.EncodeToString(sum[:])
	t.Cleanup(func() { langPackSum = old })
}

func packInstalled(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".local/share/corral/code-server/extensions", LangPackID+"-"+LangPackVersion))
	return err == nil
}

func TestLangPackInstallFromGateway(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	log := fakeCodeServer(t, home)
	withPackSum(t, "the pack")
	if r := runIn(t, home, LangPackCommand("4.138.0", FromStdin, ""), "not the pack"); r["error"] != "checksum" || packInstalled(home) {
		t.Fatalf("wrong pack: %v", r)
	}
	if r := runIn(t, home, LangPackCommand("4.138.0", FromStdin, ""), "the pack"); r["result"] != "ok" || !packInstalled(home) {
		t.Fatalf("install: %v", r)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--install-extension") || !strings.Contains(string(b), "--extensions-dir") {
		t.Fatalf("code-server called with: %s", b)
	}
	if m, _ := filepath.Glob(filepath.Join(home, ".local/state/corral/langpack-*")); len(m) != 0 {
		t.Fatalf("staging left: %v", m)
	}
	os.Remove(log)
	if r := runIn(t, home, LangPackCommand("4.138.0", FromStdin, ""), "anything"); r["result"] != "ok" {
		t.Fatalf("installed already: %v", r)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "--install-extension") {
		t.Fatal("code-server run although the pack is installed")
	}
	if strings.Contains(string(b), "--uninstall-extension") {
		t.Fatal("uninstalled before the first installation")
	}
	// A damaged copy is uninstalled, then installed again.
	dir := filepath.Join(home, ".local/share/corral/code-server/extensions", LangPackID+"-"+LangPackVersion)
	os.Remove(filepath.Join(dir, "ok"))
	if r := runIn(t, home, LangPackCommand("4.138.0", FromStdin, ""), "the pack"); r["result"] != "ok" {
		t.Fatalf("repair: %v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "ok")); err != nil {
		t.Fatal("damaged pack not installed again")
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "--uninstall-extension "+LangPackID) {
		t.Fatalf("damaged pack not uninstalled first: %s", b)
	}
}

func TestLangPackDownloads(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	fakeCodeServer(t, home)
	withPackSum(t, "downloaded pack")
	pack := filepath.Join(t.TempDir(), "pack.vsix")
	os.WriteFile(pack, []byte("downloaded pack"), 0o600)
	path, curlLog := fakeCurl(t, pack, "")
	if r := runIn(t, home, LangPackCommand("4.138.0", FromDownload, LangPackURL), "", path); r["result"] != "ok" || !packInstalled(home) {
		t.Fatalf("download install: %v", r)
	}
	if b, _ := os.ReadFile(curlLog); !strings.Contains(string(b), "--proto =https") || !strings.Contains(string(b), LangPackURL) {
		t.Fatalf("curl called with: %s", b)
	}
	// Not without code-server.
	if r := runIn(t, t.TempDir(), LangPackCommand("4.138.0", FromDownload, LangPackURL), "", path); r["error"] != "notinstalled" {
		t.Fatalf("without code-server: %v", r)
	}
}

// code-server starts in Chinese only once the pack is there.
func TestStartWithLocale(t *testing.T) {
	needSh(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 for the fake server")
	}
	home := t.TempDir()
	log := fakeCodeServer(t, home)
	sock := filepath.Join(home, ".local/state/corral/code-server.sock")
	if r := runIn(t, home, StartCommand("4.138.0", sock, "i1", op(), LangPackLocale), ""); r["result"] != "started" {
		t.Fatalf("start: %v", r)
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "--locale") {
		t.Fatalf("locale passed without the pack: %s", b)
	}
	runIn(t, home, ForceStopCommand(sock, op()), "")
	os.Remove(log)
	os.MkdirAll(filepath.Join(home, ".local/share/corral/code-server/extensions", LangPackID+"-"+LangPackVersion), 0o700)
	if r := runIn(t, home, StartCommand("4.138.0", sock, "i2", op(), LangPackLocale), ""); r["result"] != "started" {
		t.Fatalf("second start: %v", r)
	}
	b, _ = os.ReadFile(log)
	if !strings.Contains(string(b), "--locale "+LangPackLocale) || !strings.Contains(string(b), home) || !strings.Contains(string(b), "node -e") {
		t.Fatalf("started with: %s", b)
	}
	runIn(t, home, ForceStopCommand(sock, op()), "")
	// The pack could not be registered: English, rather than half Chinese.
	os.Remove(log)
	if r := runIn(t, home, StartCommand("4.138.0", sock, "i3", op(), LangPackLocale), "", "FAKE_NODE_EXIT=1"); r["result"] != "started" {
		t.Fatalf("third start: %v", r)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "--locale") {
		t.Fatalf("locale passed although registering failed: %s", b)
	}
	runIn(t, home, ForceStopCommand(sock, op()), "")
}

// The registration script, run by a real node when there is one, writes the
// entry VS Code reads, and leaves everything alone for a pack or a record it
// cannot use.
func TestLangPackRegistration(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	data := t.TempDir()
	ext := filepath.Join(data, "extensions")
	dir := filepath.Join(ext, LangPackID+"-"+LangPackVersion)
	os.MkdirAll(filepath.Join(dir, "translations"), 0o700)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"1.131.0","contributes":{"localizations":[{"languageId":"zh-cn","languageName":"Chinese Simplified","localizedLanguageName":"中文(简体)","translations":[{"id":"vscode","path":"./translations/main.i18n.json"}]}]}}`), 0o600)
	record := filepath.Join(data, "languagepacks.json")
	register := func(args ...string) error {
		return exec.Command(node, append([]string{"-e", langPackRegister, data, ext, LangPackID, LangPackVersion, LangPackLocale}, args...)...).Run()
	}
	unchanged := func(what, want string) {
		t.Helper()
		if err := register(); err == nil {
			t.Errorf("%s: registered", what)
		}
		if b, _ := os.ReadFile(record); string(b) != want {
			t.Errorf("%s: record now %s", what, b)
		}
	}
	os.WriteFile(record, []byte(`{"de":{"hash":"x"}}`), 0o600)
	unchanged("a translation file missing", `{"de":{"hash":"x"}}`)
	if register("check") == nil {
		t.Error("check passed with a translation file missing")
	}
	for _, bad := range []string{"", `{"version":"1.0.0","cont`, `{"version":"1.0.0"}`, `{"contents":[]}`, `{"contents":null}`} {
		os.WriteFile(filepath.Join(dir, "translations", "main.i18n.json"), []byte(bad), 0o600)
		if register("check") == nil {
			t.Errorf("check passed with translations %q", bad)
		}
	}
	os.WriteFile(filepath.Join(dir, "translations", "main.i18n.json"), []byte(`{"version":"1.0.0","contents":{}}`), 0o600)
	if err := register("check"); err != nil {
		t.Fatalf("check: %v", err)
	}
	if b, _ := os.ReadFile(record); string(b) != `{"de":{"hash":"x"}}` {
		t.Fatalf("check wrote the record: %s", b)
	}
	for _, bad := range []string{`{"de":`, `[]`, `null`} {
		os.WriteFile(record, []byte(bad), 0o600)
		unchanged("record "+bad, bad)
	}
	os.WriteFile(record, []byte(`{"de":{"hash":"x"}}`), 0o600)
	if err := register(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(record)
	got := string(b)
	if !strings.Contains(got, `"zh-cn"`) || !strings.Contains(got, `"de"`) || !strings.Contains(got, filepath.Join(dir, "translations", "main.i18n.json")) ||
		!strings.Contains(got, `"hash":"3351ebc019e29cf85f24c1db4cd3c4c3"`) {
		t.Fatalf("languagepacks.json: %s", got)
	}
	// No record yet: one is made.
	os.Remove(record)
	if err := register(); err != nil {
		t.Fatal(err)
	}
	// A pack without the locale is not used.
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"1.131.0","contributes":{"localizations":[{"languageId":"de","translations":[{"id":"vscode","path":"./translations/main.i18n.json"}]}]}}`), 0o600)
	if register("check") == nil {
		t.Error("check passed for a pack without zh-cn")
	}
}

// An installer that hangs is ended, with whatever it started, once its time
// is up.
func TestLangPackInstallBounded(t *testing.T) {
	needSh(t)
	home := t.TempDir()
	fakeCodeServer(t, home)
	withPackSum(t, "the pack")
	old := langPackInstallSeconds
	langPackInstallSeconds = 2
	t.Cleanup(func() { langPackInstallSeconds = old })
	child := filepath.Join(t.TempDir(), "child")
	start := time.Now()
	r := runIn(t, home, LangPackCommand("4.138.0", FromStdin, ""), "the pack", "FAKE_HANG="+child)
	if r["error"] != "install" || time.Since(start) > 15*time.Second {
		t.Fatalf("hanging install: %v after %v", r, time.Since(start))
	}
	b, _ := os.ReadFile(child)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("child pid %q", b)
	}
	for i := 0; i < 50 && running(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if running(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("the installer's child survived")
	}
}

// running is true for a process that exists and has not ended (a killed
// one nobody has reaped yet stays behind as a zombie).
func running(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true // (no /proc: as far as can be told)
	}
	st := string(b)
	if i := strings.LastIndex(st, ") "); i >= 0 && i+2 < len(st) {
		return st[i+2] != 'Z'
	}
	return true
}
