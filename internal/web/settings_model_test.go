package web

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The default model put back, the rest of the settings as they are.
func TestWithModel(t *testing.T) {
	base := "{\n  \"permissions\": {\n    \"allow\": [\"Bash(ls)\"]\n  },\n  \"effortLevel\": \"high\",\n  \"modelSettings\": {\n    \"claude-opus-5-5\": {\n      \"effortLevel\": \"high\"\n    }\n  },\n  \"advisorModel\": \"fable\""
	cases := []struct {
		name, after string
		before      string // ("" none)
	}{
		{"changed", base + ",\n  \"model\": \"sonnet\"\n}\n", `"opus"`},
		{"added at the end", base + ",\n  \"model\": \"sonnet\"\n}\n", ""},
		{"added first", "{\n  \"model\": \"sonnet\",\n" + base[2:] + "\n}\n", ""},
		{"taken out", base + "\n}\n", `"opus"`},
		{"as it was", base + ",\n  \"model\": \"opus\"\n}\n", `"opus"`},
	}
	for _, c := range cases {
		out, err := withModel([]byte(c.after), json.RawMessage(c.before), c.before != "")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Errorf("%s: not JSON: %v\n%s", c.name, err, out)
			continue
		}
		got, ok := m["model"]
		if c.before == "" && ok || c.before != "" && string(got) != c.before {
			t.Errorf("%s: model %s %v\n%s", c.name, got, ok, out)
		}
		if string(m["advisorModel"]) != `"fable"` || len(m) != 4+boolInt(c.before != "") {
			t.Errorf("%s: the rest changed: %s", c.name, out)
		}
	}
	// (Not an object: refused, nothing to write.)
	if _, err := withModel([]byte("[1]"), json.RawMessage(`"opus"`), true); err == nil {
		t.Error("an array taken")
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// review 236: a member in the middle, an empty object, a key twice.
func TestWithModelEdges(t *testing.T) {
	out, err := withModel([]byte(`{"x":1,"model":"new","y":2}`), nil, false)
	if err != nil || string(out) != `{"x":1,"y":2}` {
		t.Errorf("middle: %s %v", out, err)
	}
	out, err = withModel([]byte(`{}`), json.RawMessage(`"old"`), true)
	var m map[string]string
	if err != nil || json.Unmarshal(out, &m) != nil || m["model"] != "old" {
		t.Errorf("empty: %s %v", out, err)
	}
	if _, _, _, _, err := topValue([]byte(`{"model":"a","model":"b"}`), "model"); err == nil {
		t.Error("a key twice taken")
	}
	if _, _, _, _, err := topValue([]byte(`{"model":"a","x":`), "model"); err == nil {
		t.Error("cut short taken")
	}
}

// The write: only over what was read, only what came whole; through a link, its file.
func TestSettingsWriteCommand(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	real := filepath.Join(home, "dotfiles-settings.json")
	link := filepath.Join(home, ".claude", "settings.json")
	was := []byte(`{"model":"sonnet"}`)
	os.WriteFile(real, was, 0o640)
	os.Symlink(real, link)
	run := func(cmd string, in []byte) error {
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "HOME="+home)
		c.Stdin = bytes.NewReader(in)
		return c.Run()
	}
	fixed := []byte(`{"model":"opus"}`)
	// (Cut short on the way: refused.)
	if err := run(settingsWriteCommand(was, fixed), fixed[:5]); err == nil {
		t.Error("a part written")
	}
	// (Changed since it was read: refused, exit 9.)
	if err := run(settingsWriteCommand([]byte(`{"other":1}`), fixed), fixed); err == nil || !strings.Contains(err.Error(), "9") {
		t.Errorf("changed meanwhile: %v", err)
	}
	if b, _ := os.ReadFile(real); !bytes.Equal(b, was) {
		t.Fatalf("written when refused: %s", b)
	}
	if err := run(settingsWriteCommand(was, fixed), fixed); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(real); !bytes.Equal(b, fixed) {
		t.Errorf("not written: %s", b)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link replaced: %v", err)
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	// (Read: the file through the link; none when there is none.)
	c := exec.Command("sh", "-c", settingsReadCommand)
	c.Env = append(os.Environ(), "HOME="+home)
	if out, _ := c.Output(); !bytes.Equal(out, fixed) {
		t.Errorf("read %q", out)
	}
	os.Remove(link)
	c = exec.Command("sh", "-c", settingsReadCommand)
	c.Env = append(os.Environ(), "HOME="+home)
	if out, _ := c.Output(); string(out) != "none" {
		t.Errorf("none: %q", out)
	}
}
