package herdr

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseKeys(t *testing.T) {
	got := ParseKeys(`onboarding = false
[theme]
name = "nord"
[ "keys" ]
# prefix = "ctrl+b"
prefix = "ctrl+a"   # comment after
previous_workspace = "prefix+up"
workspace_picker = ["prefix+w", "ctrl+w"]
rename_tab = ""
rename_pane = 'prefix+T'
help = "prefix+#"
settings = []
close_tab = [
  "prefix+shift+x",
]
close_pane = "prefix+x"
goto = "prefix+$(reboot)"
toggle_sidebar = prefix+b
[keys.indexed]
tabs = "ctrl"
[[keys.command]]
key = "prefix+alt+g"
[ui]
resize_mode = "prefix+s"
`)
	want := map[string]string{"prefix": "ctrl+a", "previous_workspace": "prefix+up", "workspace_picker": "prefix+w", "rename_tab": "",
		"rename_pane": "prefix+T", "help": "prefix+#", "settings": "", "close_tab": "", "goto": "prefix+$(reboot)", "toggle_sidebar": ""} // (goto: data only; the browser cannot press it and leaves it out)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(ParseKeys("")) != 0 {
		t.Fatal("empty file: no bindings")
	}
}

// KeysCommand tells a config that is not there (success, nothing) from one
// that cannot be read, or cannot be told there or not (failure).
func TestKeysCommandFailsWhenUnreadable(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil || os.Geteuid() == 0 {
		t.Skip("needs sh, and permissions that apply (not root)")
	}
	dir := t.TempDir()
	run := func(path string) (string, error) {
		args := strings.TrimSuffix(strings.TrimPrefix(KeysCommand, "sh -c '"), "'")
		cmd := exec.Command("sh", "-c", args)
		cmd.Env = append(os.Environ(), "HERDR_CONFIG_PATH="+path)
		out, err := cmd.Output()
		return string(out), err
	}
	os.WriteFile(filepath.Join(dir, "ok.toml"), []byte("[keys]\n"), 0o600)
	if out, err := run(filepath.Join(dir, "ok.toml")); err != nil || out != "[keys]\n" {
		t.Fatalf("readable: %q %v", out, err)
	}
	if out, err := run(filepath.Join(dir, "none", "herdr", "config.toml")); err != nil || out != "" {
		t.Fatalf("not there: %q %v", out, err)
	}
	os.WriteFile(filepath.Join(dir, "locked.toml"), []byte("x"), 0)
	if _, err := run(filepath.Join(dir, "locked.toml")); err == nil {
		t.Fatal("unreadable file: no failure")
	}
	os.MkdirAll(filepath.Join(dir, "shut", "inner"), 0o700)
	os.Chmod(filepath.Join(dir, "shut"), 0)
	defer os.Chmod(filepath.Join(dir, "shut"), 0o700)
	if _, err := run(filepath.Join(dir, "shut", "inner", "herdr", "config.toml")); err == nil {
		t.Fatal("under a folder that cannot be searched: no failure")
	}
}
