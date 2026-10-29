package convo

import (
	"encoding/json"
	"strings"
	"testing"
)

// What a "!" command printed: a shell part, its text as printed (Claude
// Code's &gt; and the like undone), a long one kept by its start and end,
// stderr apart.
func TestClaudeShellOutput(t *testing.T) {
	n := 0
	row := func(content string) string {
		n++
		b, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u" + string(rune('0'+n)), "timestamp": "2026-10-29T01:00:00Z", "message": map[string]any{"role": "user", "content": content}})
		return string(b) + "\n"
	}
	long := strings.Repeat("line &gt;&gt;&gt; x &amp; y\n", 2000)
	c := NewClaude()
	c.Feed([]byte(row("<bash-input>python3 t.py</bash-input>") + row("<bash-stdout>"+long+"</bash-stdout><bash-stderr>warn: &lt;x&gt;</bash-stderr>") + row("<bash-stdout></bash-stdout><bash-stderr></bash-stderr>")))
	es := c.Since(0)
	if len(es) != 2 {
		for _, e := range es {
			t.Logf("%s %+v", e.Role, e.Parts)
		}
		t.Fatalf("%d entries (nothing printed: none)", len(es))
	}
	if es[0].Role != RoleUser || es[0].Parts[0].Text != "! python3 t.py" {
		t.Fatalf("the command: %+v", es[0])
	}
	p := es[1].Parts[0]
	if es[1].Role != RoleNote || p.Kind != KindShell || p.Shell == nil {
		t.Fatalf("not a shell part: %+v", es[1])
	}
	sh := p.Shell
	if !strings.HasPrefix(sh.Out, "line >>> x & y\n") || strings.Contains(sh.Out+sh.OutTail, "&gt;") {
		t.Fatalf("not as printed: %.40q", sh.Out)
	}
	if !sh.Truncated || sh.OutTail == "" || len(sh.Out)+len(sh.OutTail) > maxResult+64 {
		t.Fatalf("a long one: start %d, end %d, cut %v", len(sh.Out), len(sh.OutTail), sh.Truncated)
	}
	if sh.Err != "warn: <x>" {
		t.Fatalf("stderr: %q", sh.Err)
	}
}
