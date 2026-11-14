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

	// (What may be a secret, by its shape: not on a lock screen.)
	for in, want := range map[string]string{
		"登录链接 https://x.example.com/login?k=0123456789abcdef0123456789abcdef 已生成":          "登录链接 https://x.example.com/login… 已生成",
		"用 https://user:pw@host.example/a/b#frag 访问":                                       "用 https://host.example/a/b… 访问",
		"key is sk-ant-api03-AbCdEf0123456789xyzXYZ0123 now":                               "key is … now",
		"这一轮消耗了 2M token 左右，secret sauce 不变":                                               "这一轮消耗了 2M token 左右，secret sauce 不变",
		"Authorization: Bearer eyJhbGciOi.payload":                                         "Authorization: …",
		"curl -H 'bearer abc123'":                                                          "curl -H 'bearer …",
		"password=hunter2 and token: abc":                                                  "password=… and token: …",
		"k=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef":               "k=…",
		"已部署 v0.2.0-steps-20261114-1248-d376905，回滚 corral:rollback-20261114-1248":          "已部署 v0.2.0-steps-20261114-1248-d376905，回滚 corral:rollback-20261114-1248",
		"会话 91df602d-a996-48a5-8d8a-b9a5d7ba136d 里 0174c2b 已推送":                            "会话 91df602d-a996-48a5-8d8a-b9a5d7ba136d 里 0174c2b 已推送",
		"token ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8 泄露了":                               "token … 泄露了",
		"commit 0174c2b, sha 2a895c3c2af0a71804159612500377e73b7c96da":                     "commit 0174c2b, sha …",
		"/home/ihainan/tmp/corral-devtools/research/leak 下的 internationalization_settings": "/home/ihainan/tmp/corral-devtools/research/leak 下的 internationalization_settings",
	} {
		if got := maskSecrets(in); got != want {
			t.Errorf("maskSecrets(%q) = %q, want %q", in, got, want)
		}
	}
}
