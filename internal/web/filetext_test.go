package web

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// The text read sends a file's words as JSON (never the file as itself,
// whatever its name: HTML and SVG are words there too), never cached; a
// file past the limit cut at a whole character and said so; one not
// UTF-8, or with a NUL near its start, refused as not text; a folder, a
// path not there, a relative one refused. Each read is audited; signed
// out, nothing.
func TestFilesText(t *testing.T) {
	e := newFileEnv(t)
	type answer struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		Text string `json:"text"`
		Cut  bool   `json:"cut"`
	}
	read := func(name string, data []byte) (int, answer, []byte, string) {
		if data != nil {
			os.WriteFile(filepath.Join(e.root, name), data, 0o644)
		}
		res, body := e.raw("GET", e.url("text", url.Values{"path": {e.root + "/" + name}}), nil, "")
		var a answer
		json.Unmarshal(body, &a)
		if res.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s headers: %v", name, res.Header)
		}
		return res.StatusCode, a, body, res.Header.Get("Content-Type")
	}
	for name, words := range map[string]string{
		"main.go":   "package main\n\nfunc main() {}\n",
		"注意.md":     "# 标题\n\n中文 text\n",
		"page.html": "<html><script>alert(1)</script></html>",
		"x.svg":     `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"empty.txt": "",
		"crlf.ini":  "a=1\r\nb=2\r\n",
	} {
		st, a, body, _ := read(name, []byte(words))
		if st != 200 || a.Text != words || a.Cut || a.Size != int64(len(words)) || a.Path != e.root+"/"+name {
			t.Errorf("%s: %d %.120q", name, st, body)
		}
	}
	// Past the limit: its beginning, cut before a character it would split.
	long := strings.Repeat("a", textMaxBytes-1) + "中" + "tail"
	st, a, _, _ := read("long.txt", []byte(long))
	if st != 200 || !a.Cut || len(a.Text) != textMaxBytes-1 || !utf8.ValidString(a.Text) || a.Size != int64(len(long)) {
		t.Errorf("long: %d cut=%v len=%d size=%d", st, a.Cut, len(a.Text), a.Size)
	}
	// Its end asked for (a command's output): the newest, from a whole line, said cut; a short one whole.
	tailOf := func(name string, data []byte) answer {
		os.WriteFile(filepath.Join(e.root, name), data, 0o644)
		_, body := e.raw("GET", e.url("text", url.Values{"path": {e.root + "/" + name}, "tail": {"1"}}), nil, "")
		var a answer
		json.Unmarshal(body, &a)
		return a
	}
	logLines := strings.Repeat("old line 中文\n", textMaxBytes/16) + "newest line\n"
	if a := tailOf("out.log", []byte(logLines)); !a.Cut || !strings.HasSuffix(a.Text, "newest line\n") || !strings.HasPrefix(a.Text, "old line 中文\n") || len(a.Text) > textMaxBytes || !utf8.ValidString(a.Text) {
		t.Errorf("tail: cut=%v len=%d start=%.20q end=%.20q", a.Cut, len(a.Text), a.Text, a.Text[max(0, len(a.Text)-20):])
	}
	if a := tailOf("short.log", []byte("one\ntwo\n")); a.Cut || a.Text != "one\ntwo\n" {
		t.Errorf("a short tail: %+v", a)
	}
	exact := strings.Repeat("b", textMaxBytes)
	if st, a, _, _ := read("exact.txt", []byte(exact)); st != 200 || a.Cut || len(a.Text) != textMaxBytes {
		t.Errorf("at the limit: %d cut=%v len=%d", st, a.Cut, len(a.Text))
	}
	for name, data := range map[string][]byte{
		"a.png":    pngOf(3, 2),
		"nul.txt":  []byte("text\x00more"),
		"gbk.txt":  {0xd6, 0xd0, 0xce, 0xc4},
		"bad.json": append([]byte(`{"a":`), 0xff, '}'),
	} {
		if st, _, body, _ := read(name, data); st != 415 || !bytes.Contains(body, []byte(`"not_text"`)) {
			t.Errorf("%s: %d %.80q", name, st, body)
		}
	}
	os.Mkdir(filepath.Join(e.root, "dir.txt"), 0o755)
	if res, _ := e.raw("GET", e.url("text", url.Values{"path": {e.root + "/dir.txt"}}), nil, ""); res.StatusCode != 400 {
		t.Errorf("a folder: %d", res.StatusCode)
	}
	// A FIFO with no writer, and a link to it: refused before it is opened (opening would block).
	if err := syscall.Mkfifo(filepath.Join(e.root, "pipe.txt"), 0o644); err == nil {
		os.Symlink(filepath.Join(e.root, "pipe.txt"), filepath.Join(e.root, "to-pipe.txt"))
		for _, n := range []string{"pipe.txt", "to-pipe.txt"} {
			done := make(chan int, 1)
			go func() {
				res, _ := e.raw("GET", e.url("text", url.Values{"path": {e.root + "/" + n}}), nil, "")
				done <- res.StatusCode
			}()
			select {
			case st := <-done:
				if st != 400 {
					t.Errorf("%s: %d", n, st)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: blocked", n)
			}
		}
	}
	if res, _ := e.raw("GET", e.url("text", url.Values{"path": {e.root + "/none.txt"}}), nil, ""); res.StatusCode != 404 {
		t.Errorf("not there: %d", res.StatusCode)
	}
	if res, _ := e.raw("GET", e.url("text", url.Values{"path": {"main.go"}}), nil, ""); res.StatusCode != 400 {
		t.Errorf("relative: %d", res.StatusCode)
	}
	n := 0
	for _, a := range auditActions(t, e.h) {
		if a == "file.text" {
			n++
		}
	}
	if n != 10 { // (the eight texts read, the two ends)
		t.Errorf("file.text audited %d times, want 10", n)
	}
	e.c.do("POST", "/api/logout", nil)
	if res, _ := e.raw("GET", e.url("text", url.Values{"path": {e.root + "/main.go"}}), nil, ""); res.StatusCode != 401 {
		t.Errorf("signed out: %d", res.StatusCode)
	}
}

func TestWholeChars(t *testing.T) {
	for in, want := range map[string]string{"abc": "abc", "ab中": "ab中", "ab\xe4\xb8": "ab", "ab\xe4": "ab", "": "", "\xf0\x9f\x98": ""} {
		if got := string(wholeChars([]byte(in))); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
