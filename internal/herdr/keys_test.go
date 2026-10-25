package herdr

import (
	"reflect"
	"testing"
)

func TestParseKeys(t *testing.T) {
	got := ParseKeys(`onboarding = false
[theme]
name = "nord"
[keys]
# prefix = "ctrl+b"
prefix = "Ctrl+A"   # comment after
previous_workspace = "prefix+up"
workspace_picker = ["prefix+w", "ctrl+w"]
rename_tab = ""
close_pane = "prefix+x"
goto = "prefix+$(reboot)"
help = prefix+?
[keys.indexed]
tabs = "ctrl"
[[keys.command]]
key = "prefix+alt+g"
[ui]
settings = "prefix+s"
`)
	want := map[string]string{"prefix": "ctrl+a", "previous_workspace": "prefix+up", "workspace_picker": "prefix+w", "rename_tab": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(ParseKeys("")) != 0 {
		t.Fatal("empty file: no bindings")
	}
}
