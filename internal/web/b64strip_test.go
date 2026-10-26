package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/convo"
)

// A message with an image, its line longer than a read: what is left once
// the image's data is out, written in parts, is the message, as JSON.
func TestB64StripKeepsTheMessage(t *testing.T) {
	data := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAA", 120_000) // ~2.9 MB of base64
	line := `{"type":"user","uuid":"u1","timestamp":"2026-10-26T01:00:00Z","message":{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}},` +
		`{"type":"text","text":"look at this one (id abc123DEF)"}]}}`
	var b b64Strip
	var out []byte
	for i := 0; i < len(line); i += 1 << 20 {
		out = b.write(out, []byte(line[i:min(len(line), i+1<<20)]))
	}
	out = b.end(out)
	if len(out) > 1000 || !json.Valid(out) {
		t.Fatalf("%d bytes, valid %v: %.300s", len(out), json.Valid(out), out)
	}
	if !strings.Contains(string(out), "look at this one (id abc123DEF)") {
		t.Fatalf("text lost: %s", out)
	}
	c := convo.NewClaude()
	c.Feed(append(out, '\n'))
	es := c.Since(0)
	if len(es) != 1 || !strings.Contains(fmt.Sprint(es[0].Parts), "look at this one") {
		t.Fatalf("entries: %+v", es)
	}
}

// Text stays, however long its runs of letters (an encoded string to look
// at, say); only "data" strings are emptied, across the parts' bounds.
func TestB64StripLeavesText(t *testing.T) {
	long := strings.Repeat("QUJD", 2000)
	s := `{"text":"` + long + ` and sha 0123456789abcdef","note":"say \"data\":\"x\"","source":{"data":"` + long + `"},"after":1}`
	var b b64Strip
	var out []byte
	for i := 0; i < len(s); i += 7 {
		out = b.write(out, []byte(s[i:min(len(s), i+7)]))
	}
	out = b.end(out)
	want := `{"text":"` + long + ` and sha 0123456789abcdef","note":"say \"data\":\"x\"","source":{"data":""},"after":1}`
	if string(out) != want {
		t.Fatalf("got %.200s", out)
	}
}

// The name looked up with grep: a row of its own, of this session.
func TestCustomTitleLine(t *testing.T) {
	if l := customTitleLine([]byte(`{"type":"custom-title","customTitle":"fix it","sessionId":"`+sidA+`"}`+"\n"), sidA); !strings.Contains(string(l), `"fix it"`) {
		t.Errorf("its own: %q", l)
	}
	for _, bad := range []string{
		`{"type":"custom-title","customTitle":"x","sessionId":"` + sidB + `"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","input":{"type":"custom-title","customTitle":"x"}}]}}`,
		`{"type":"custom-title","customTitle":"x"`,
		``,
	} {
		if l := customTitleLine([]byte(bad), sidA); l != nil {
			t.Errorf("taken: %s -> %s", bad, l)
		}
	}
}

// Other "data" strings stay as they were: short, with escaped quotes in
// them, letters then words, a field whose name ends in "data".
func TestB64StripKeepsOtherData(t *testing.T) {
	long := strings.Repeat("QUJD", 2000)
	for _, kept := range []string{
		`{"input":{"data":"say \"hello\" now"},"metadata":"` + long + `"}`,
		`{"data":"` + strings.Repeat("ab", 100) + ` and then words"}`,
		`{"data":""}`,
	} {
		var b b64Strip
		var got []byte
		for i := 0; i < len(kept); i += 5 {
			got = b.write(got, []byte(kept[i:min(len(kept), i+5)]))
		}
		if got = b.end(got); string(got) != kept {
			t.Errorf("changed: %.120s -> %.120s", kept, got)
		}
	}
}

// Codex's images: an image_url data address, its base64 out, its head kept.
func TestB64StripImageURL(t *testing.T) {
	data := strings.Repeat("iVBORw0KGgo", 3000)
	line := `{"type":"input_image","image_url":"data:image/png;base64,` + data + `"},{"type":"input_text","text":"look","image_url":"https://example.com/a.png"}`
	var b b64Strip
	var out []byte
	for i := 0; i < len(line); i += 4096 {
		out = b.write(out, []byte(line[i:min(len(line), i+4096)]))
	}
	out = b.end(out)
	want := `{"type":"input_image","image_url":"data:image/png;base64,"},{"type":"input_text","text":"look","image_url":"https://example.com/a.png"}`
	if string(out) != want {
		t.Fatalf("got %.300s", out)
	}
}
