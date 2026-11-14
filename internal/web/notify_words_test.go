package web

import (
	"strings"
	"testing"
)

// A finished alert's line: the agent's last words, Markdown's marks out, cut
// at a sentence's end where one falls early enough; a terminal title only
// where it says something.
func TestPlainWords(t *testing.T) {
	for in, want := range map[string]string{
		"这一轮又上线了 5 项，当前版本 147a98a： - **权限模式菜单**：每个模式加了图标":       "这一轮又上线了 5 项，当前版本 147a98a： 权限模式菜单：每个模式加了图标",
		"Fixed `foo()` in [notify.go](internal/web/notify.go).": "Fixed foo() in notify.go.",
		"## Done 1. built 2. deployed": "Done built deployed",
	} {
		if got := plainWords(in); got != want {
			t.Errorf("plainWords(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("长", 50) + "。" + strings.Repeat("后", 200)
	if got := plainWords(long); got != strings.Repeat("长", 50)+"。" {
		t.Errorf("cut at the sentence: %q", got)
	}
	if got := []rune(plainWords(strings.Repeat("字", 300))); len(got) != doneSaidMax || got[len(got)-1] != '…' {
		t.Errorf("cut with an ellipsis: %d runes", len(got))
	}
	for title, says := range map[string]bool{"AI": false, "codex": false, "Claude Code": false, "✳ 修复下载按钮没反应": true} {
		if titleSays(title, "codex") != says {
			t.Errorf("titleSays(%q) = %v", title, !says)
		}
	}
}
