package convo

import (
	"bytes"
	"encoding/base64"
	"testing"
)

// A picture the parser tells of is had again from its row as it counts it:
// in a message's content, or a message sent while at work (its prompt).
func TestImageOfAsParsed(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfirst")
	jpg := []byte("\xff\xd8\xff\xe0second")
	b := func(d []byte) string { return base64.StdEncoding.EncodeToString(d) }
	row1 := `{"type":"user","uuid":"11111111-2222-3333-4444-555555555555","message":{"role":"user","content":[{"type":"text","text":"看这两张"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b(png) + `"}},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + b(jpg) + `"}}]}}`
	row2 := `{"type":"attachment","uuid":"AAAAAAAA-2222-3333-4444-555555555555","attachment":{"type":"queued_command","commandMode":"prompt","imagePasteIds":[27],"prompt":[{"type":"text","text":"[Image #27] 还有这张"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + b(jpg) + `"}}]}}`
	c := NewClaude()
	c.Feed(lines(row1, row2))
	var from []string
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Kind == KindImage {
				from = append(from, p.From)
			}
		}
	}
	want := []string{"11111111-2222-3333-4444-555555555555.0", "11111111-2222-3333-4444-555555555555.1", "aaaaaaaa-2222-3333-4444-555555555555.0"}
	if len(from) != 3 || from[0] != want[0] || from[1] != want[1] || from[2] != want[2] {
		t.Fatalf("from: %q", from)
	}
	for i, row := range []string{row1, row1, row2} {
		uuid, n, ok := ImageAt(from[i])
		if !ok {
			t.Fatalf("not read: %q", from[i])
		}
		media, data, err := ImageOf([]byte(row), uuid, n)
		wantData, wantMedia := []byte(jpg), "image/jpeg"
		if i == 0 {
			wantData, wantMedia = png, "image/png"
		}
		if err != nil || media != wantMedia || !bytes.Equal(data, wantData) {
			t.Errorf("%d: %q %q %v", i, media, data, err)
		}
	}
	// (Not that row, or no such picture of it.)
	if _, _, err := ImageOf([]byte(row1), "aaaaaaaa-2222-3333-4444-555555555555", 0); err == nil {
		t.Error("another row's uuid")
	}
	if _, _, err := ImageOf([]byte(row2), "aaaaaaaa-2222-3333-4444-555555555555", 1); err != ErrNoImage {
		t.Errorf("past its pictures: %v", err)
	}
	for _, bad := range []string{"", "x.0", "11111111-2222-3333-4444-555555555555", "11111111-2222-3333-4444-555555555555.64", "11111111-2222-3333-4444-555555555555.-1", "../etc/passwd.0"} {
		if _, _, ok := ImageAt(bad); ok {
			t.Errorf("taken: %q", bad)
		}
	}
}
