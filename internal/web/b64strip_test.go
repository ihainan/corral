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

// Short runs (words, ids, a hash) stay.
func TestB64StripLeavesText(t *testing.T) {
	s := `{"text":"` + strings.Repeat("a", b64Run) + ` and sha 0123456789abcdef"}`
	var b b64Strip
	if out := string(b.end(b.write(nil, []byte(s)))); out != s {
		t.Fatalf("changed: %.80s", out)
	}
}
