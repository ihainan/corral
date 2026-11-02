package convo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A picture the person sent by its path: Claude Code's "[Image: source: …]"
// row, hanging on the message through an attachment's row, says which file
// each was, in order; read in a later Feed, the message's entry changes for
// it. A row hanging on something else, a count not the message's, and a
// relative path are not taken.
func TestClaudeImageSources(t *testing.T) {
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":[{"type":"text","text":"整体：\n\n[Image #5]\n\n具体：\n\n[Image #6]"},`+img+`,`+img+`]}}`,
		`{"type":"attachment","uuid":"x1","parentUuid":"u1","attachment":{"type":"total_tokens_reminder"}}`,
	))
	rev := c.Rev()
	c.Feed(lines(
		`{"type":"user","uuid":"m1","parentUuid":"x1","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"[Image: source: /home/u/.cache/corral/uploads/a.png]"},{"type":"text","text":"[Image: source: /home/u/.cache/corral/uploads/b.png]"}]}}`,
	))
	ch := c.Since(rev)
	if len(ch) != 1 || ch[0].ID != "u1" {
		t.Fatalf("changed: %+v", ch)
	}
	if p := paths(ch[0]); len(p) != 2 || p[0] != "/home/u/.cache/corral/uploads/a.png" || p[1] != "/home/u/.cache/corral/uploads/b.png" {
		t.Fatalf("paths: %q", p)
	}

	// Hanging on an answer: not a message of pictures — nothing taken.
	c.Feed(lines(
		`{"type":"user","uuid":"u2","parentUuid":"m1","message":{"role":"user","content":[{"type":"text","text":"[Image #1] 看"},`+img+`]}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u2","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"user","uuid":"m2","parentUuid":"a1","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"[Image: source: /home/u/c.png]"}]}}`,
	))
	if p := paths(find(c, "u2")); p[0] != "" {
		t.Fatalf("taken through an answer: %q", p)
	}
	// Two sources for one picture: none taken.
	c.Feed(lines(
		`{"type":"user","uuid":"m3","parentUuid":"u2","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"[Image: source: /home/u/d.png]"},{"type":"text","text":"[Image: source: /home/u/e.png]"}]}}`,
	))
	if p := paths(find(c, "u2")); p[0] != "" {
		t.Fatalf("a count not the message's: %q", p)
	}
	// A relative path: its picture keeps its place, without one.
	c.Feed(lines(
		`{"type":"user","uuid":"u3","parentUuid":"m3","message":{"role":"user","content":[{"type":"text","text":"[Image #1] [Image #2]"},`+img+`,`+img+`]}}`,
		`{"type":"user","uuid":"m4","parentUuid":"u3","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"[Image: source: shot.png]"},{"type":"text","text":"[Image: source: /home/u/f.png]"}]}}`,
	))
	if p := paths(find(c, "u3")); len(p) != 2 || p[0] != "" || p[1] != "/home/u/f.png" {
		t.Fatalf("relative: %q", p)
	}
}

// The log read in parts (its end first, then the part before put before
// it): a source read before its message is told it once that part comes;
// a message of the part before, its source read later, likewise.
func TestClaudeImageSourcesPrepend(t *testing.T) {
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`
	msg := `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":[{"type":"text","text":"[Image #1]"},` + img + `]}}`
	att := `{"type":"attachment","uuid":"x1","parentUuid":"u1","attachment":{"type":"total_tokens_reminder"}}`
	src := `{"type":"user","uuid":"m1","parentUuid":"x1","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"[Image: source: /home/u/a.png]"}]}}`

	// The source in the later part (read first), the message and its attachment before.
	c, o := NewClaude(), NewClaude()
	c.Feed(lines(src))
	o.Feed(lines(msg, att))
	rev := c.Rev()
	c.Prepend(o)
	got := find(c, "u1")
	if p := paths(got); len(p) != 1 || p[0] != "/home/u/a.png" || got.Rev <= rev {
		t.Fatalf("source before its message: %q rev %d", p, got.Rev)
	}

	// The message and its attachment in the part before, the source read after it was put before.
	c, o = NewClaude(), NewClaude()
	c.Feed(lines(`{"type":"assistant","uuid":"a0","parentUuid":"x1","message":{"content":[{"type":"text","text":"…"}]}}`))
	o.Feed(lines(msg, att))
	c.Prepend(o)
	rev = c.Rev()
	c.Feed(lines(src))
	got = find(c, "u1")
	if p := paths(got); len(p) != 1 || p[0] != "/home/u/a.png" || got.Rev <= rev {
		t.Fatalf("source after the part before: %q rev %d", p, got.Rev)
	}
}

func paths(e Entry) []string {
	var out []string
	for _, p := range e.Parts {
		if p.Kind == KindImage {
			out = append(out, p.Path)
		}
	}
	return out
}

func find(c *Claude, id string) Entry {
	for _, e := range c.Since(0) {
		if e.ID == id {
			return e
		}
	}
	return Entry{}
}

// Real logs (CORRAL_CLAUDE_LOGS, a glob): of a log read whole (not past
// maxEntries), every picture its source rows tell is linked to that path.
// (A message queued while the agent worked gets no source row: not counted.)
func TestClaudeImageSourcesReal(t *testing.T) {
	pat := os.Getenv("CORRAL_CLAUDE_LOGS")
	if pat == "" {
		t.Skip("CORRAL_CLAUDE_LOGS not set")
	}
	files, _ := filepath.Glob(pat)
	told, linked, logs := 0, 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(b), "[Image: source: /") {
			continue
		}
		c := NewClaude()
		c.Feed(append(b, '\n'))
		all := c.Since(0)
		if len(all) >= maxEntries {
			continue
		}
		logs++
		n := 0
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, `"isMeta":true`) && strings.Contains(l, `"type":"user"`) {
				n += strings.Count(l, "[Image: source: /")
			}
		}
		m := 0
		for _, e := range all {
			for _, p := range e.Parts {
				if p.Kind == KindImage && p.Path != "" {
					m++
				}
			}
		}
		if m != n {
			t.Errorf("%s: %d told, %d linked", filepath.Base(f), n, m)
		}
		told, linked = told+n, linked+m
	}
	t.Logf("%d logs: %d sources told, %d pictures linked", logs, told, linked)
}
