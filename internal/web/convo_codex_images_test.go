package web

import (
	"reflect"
	"testing"
)

// A Codex message cut where pictures' paths stand alone (each pasted on its
// own: Codex takes it for a picture).
func TestCodexPasteParts(t *testing.T) {
	const p = "/home/alice/up/shot.png"
	for _, c := range []struct {
		text string
		want []string
	}{
		{"看看这张 " + p + " 说说", []string{"看看这张 ", p, " 说说"}},
		{p + " 看看", []string{p, " 看看"}},
		{"看看 '/home/alice/my shots/a b.png'", []string{"看看 ", "'/home/alice/my shots/a b.png'"}},
		{p + " " + p + " 两张", []string{p, " ", p, " 两张"}},
		{"no picture here", nil},
		{"a path/to/x.pngfile word", nil}, // (not standing alone)
		{"see " + p + "x and", nil},       // (a word goes on after it)
		{"run /tmp/a.txt please", nil},    // (not a picture)
	} {
		parts := codexPasteParts(c.text)
		var got []string
		at := 0
		for _, n := range parts {
			got = append(got, c.text[at:at+n])
			at += n
		}
		if parts != nil && at != len(c.text) {
			t.Errorf("%q: parts %v do not cover it", c.text, parts)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %q, want %q", c.text, got, c.want)
		}
	}
}

// Codex's input with a picture pasted in it: "[Image #1]" where its path was.
func TestCodexTypedInPictures(t *testing.T) {
	const p = "/home/alice/up/shot.png"
	msg := p + " 看看这张图说说"
	if !codexTypedIn(codexScreen("› [Image #1] 看看这张图说说"), msg, true) {
		t.Error("the picture and the words: whole")
	}
	if !codexTypedIn(codexScreen("› [Image #1] 看看这张图说说"), msg, false) {
		t.Error("the picture and the words: still typed")
	}
	if codexTypedIn(codexScreen("› [Image #1] 别的话"), msg, true) || codexTypedIn(codexScreen("› [Image #1] 别的话"), msg, false) {
		t.Error("the picture, other words: not it")
	}
	two := p + " " + p + " 两张都看看"
	if !codexTypedIn(codexScreen("› [Image #1]  [Image #2]  两张都看看"), two, true) {
		t.Error("two pictures and words: whole")
	}
}
