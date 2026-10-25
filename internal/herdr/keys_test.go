package herdr

import (
	"reflect"
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
