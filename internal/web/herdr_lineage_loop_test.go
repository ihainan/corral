package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// Chat 47 (11-25): tabs a Claude Code opened for its agy workers, in the ways
// it wrote them, that the lineage scan missed — a loop over two worktrees (its
// full results, then filtered to the pane ids), and one opened after two
// heredocs had written files. Each child is found now; what was refused still is.

func claudeCall(id, cmd, result string) string {
	use, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}}}})
	res, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": result}}}})
	return string(use) + "\n" + string(res) + "\n"
}

func tabCreated(pane, term string) string {
	return `{"id":"cli:tab:create","result":{"root_pane":{"pane_id":"` + pane + `","tab_id":"w2:t9","terminal_id":"` + term + `","workspace_id":"w2"},"tab":{"label":"x","tab_id":"w2:t9","workspace_id":"w2"},"type":"tab_created"}}`
}

func panesOf(ds []lineageDiscovery) string {
	var p []string
	for _, d := range ds {
		p = append(p, d.ChildPane)
	}
	return strings.Join(p, ",")
}

func TestLineageLoopsAndHeredocs(t *testing.T) {
	cases := []struct{ name, cmd, out, want string }{
		{"loop, full results",
			`for x in a c; do T=$HOME/tmp/impl-$x; herdr tab create --workspace $HERDR_WORKSPACE_ID --cwd $HOME/wt/P-$x --label blog-$x --env TMPDIR=$T --no-focus 2>&1 | tail -1; done; herdr tab list --workspace $HERDR_WORKSPACE_ID 2>&1 | grep -oE '"(tab_id|label|pane_id)":"[^"]*"' | head`,
			tabCreated("w2:p3", "term_3") + "\n" + tabCreated("w2:p4", "term_4") + "\n" + `"tab_id":"w2:t3" "label":"blog-a" "pane_id":"w2:p3"`,
			"w2:p3,w2:p4"},
		{"loop, pane ids only",
			`cd /repo && for x in b d; do git worktree add -q -b wp/$x ~/wt/P-$x 51b7e57 && mkdir -p ~/tmp/impl-$x && herdr tab create --workspace $HERDR_WORKSPACE_ID --cwd $HOME/wt/P-$x --label blog-$x --no-focus 2>&1 | grep -oE '"pane_id":"[^"]*"' | head -1; done`,
			`"pane_id":"w2:p5"` + "\n" + `"pane_id":"w2:p6"`,
			"w2:p5,w2:p6"},
		{"after two heredocs",
			"D=~/tmp/p; python3 - <<'EOF'\nimport os\nopen(os.path.expanduser('~/tmp/p/rules.md'),'w').write('x')\nEOF\ncat > $D/brief-1.md <<'EOF'\n# brief\nWork in `~/wt/P-g`; reply with one line.\nEOF\nherdr tab create --workspace w2 --cwd ~/wt/P-g --label blog-g --no-focus 2>&1 | tail -2",
			tabCreated("w2:p8", "term_8"),
			"w2:p8"},
		{"a loop over a command's words: once",
			`for x in $(ls ~/wt); do herdr tab create --cwd ~/wt/$x --no-focus 2>&1 | grep -oE '"pane_id":"[^"]*"'; done`,
			`"pane_id":"w2:p5"` + "\n" + `"pane_id":"w2:p6"`,
			""},
		{"a heredoc naming herdr", "cat > f <<'EOF'\nherdr tab create\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"a heredoc to a shell", "sh <<'EOF'\necho hi\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"a heredoc to a remote", "ssh box <<'EOF'\necho hi\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"a heredoc to herdr", "herdr api call <<'EOF'\n{}\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"a heredoc never ended", "cat > f <<'EOF'\nhello\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"a heredoc spoofing a result", "cat <<'EOF'\n{\"type\":\"tab_created\"}\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"an unquoted heredoc running a command", "cat > f <<EOF\n$(id)\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"an unquoted heredoc with backticks", "cat > f <<EOF\n`id`\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"an unquoted heredoc of plain words", "cat > f <<EOF\nhello $HOME\nEOF\nherdr tab create --no-focus", tabCreated("w2:p8", "term_8"), "w2:p8"},
		{"backticks outside a heredoc", "cat > f <<'EOF'\nhi\nEOF\necho `id`; herdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
		{"a here-string", "cat <<< hi; herdr tab create --no-focus", tabCreated("w2:p8", "term_8"), ""},
	}
	for _, c := range cases {
		ex := newLineageExtractor("claude", "", "")
		got := panesOf(ex.Feed([]byte(claudeCall("toolu_x", c.cmd, c.out))))
		if got != c.want {
			t.Errorf("%s: found %q, want %q", c.name, got, c.want)
		}
	}
}

func TestForLoopWords(t *testing.T) {
	for in, want := range map[string]int{"for x in a c": 2, "for x in b d e": 3, "for x in $(ls)": 1, "for x in *": 1, "for x in 1 2 3 4 5 6 7 8 9": 1, "echo for x in a": 0} {
		n, _ := forLoopWords(in)
		if n != want {
			t.Errorf("%q: %d, want %d", in, n, want)
		}
	}
}
