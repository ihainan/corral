package web

import "testing"

// The screen as shown, its rows the terminal wrapped joined as the terminal
// tells them, and no more than the screen (10-31; review496: herdr's joined
// lines go back above the screen).
func TestUnwrapScreen(t *testing.T) {
	const visible = "or you\n   3. No\n ❯ 1. Yes\n   2. Yes, and switch to auto mode · auto mode handles these prompts f\nor you\n   3. No\n\n Esc to cancel · Tab to amend\n\n"
	// (joined: an old prompt above the screen too, its rows gone up.)
	const joined = "old question?\n   2. Yes, and switch to auto mode · auto mode handles these prompts for you\n   3. No\n ❯ 1. Yes\n   2. Yes, and switch to auto mode · auto mode handles these prompts for you\n   3. No\n\n Esc to cancel · Tab to amend\n"
	got, ok := unwrapScreen(visible, joined)
	want := "or you\n   3. No\n ❯ 1. Yes\n   2. Yes, and switch to auto mode · auto mode handles these prompts for you\n   3. No\n\n Esc to cancel · Tab to amend"
	if !ok || got != want {
		t.Fatalf("joined: %v\n%q\nwant\n%q", ok, got, want)
	}
	// Nothing wrapped: the screen, as it is.
	plain := " Do you want to proceed?\n ❯ 1. Yes\n   3. No"
	if got, ok := unwrapScreen(plain, "earlier\n"+plain+"\n"); !ok || got != plain {
		t.Fatalf("nothing wrapped: %v %q", ok, got)
	}
	// Cut at a space, the row's end shown without it.
	if got, ok := unwrapScreen("a b c d\ne f\nlast", "a b c d e f\nlast"); !ok || got != "a b c d e f\nlast" {
		t.Fatalf("cut at a space: %v %q", ok, got)
	}
	// Wide letters cut in two rows.
	if got, ok := unwrapScreen("硬件选型、部署过程与性能实测见上\n篇。\n尾", "x\n硬件选型、部署过程与性能实测见上篇。\n尾\n"); !ok || got != "硬件选型、部署过程与性能实测见上篇。\n尾" {
		t.Fatalf("wide: %v %q", ok, got)
	}
	// Another screen meanwhile: the screen as shown.
	if got, ok := unwrapScreen(visible, "something else\nentirely\n"); ok || got != visible {
		t.Fatalf("not matching: %v %q", ok, got)
	}
	// joined shorter than the screen: the screen as shown.
	if _, ok := unwrapScreen("a\nb\nc", "c"); ok {
		t.Fatal("joined shorter than the screen taken")
	}
	if _, ok := unwrapScreen("", "a"); ok {
		t.Fatal("an empty screen taken")
	}
	// review497: letters differ but for spaces — not the same line; a blank
	// row is not the end of a line before it.
	if got, ok := unwrapScreen("rm -rf / tmp/cache\nlast", "rm -rf /tmp/cache\nlast"); ok || got != "rm -rf / tmp/cache\nlast" {
		t.Fatalf("spaces within a row: %v %q", ok, got)
	}
	if _, ok := unwrapScreen("A\n\nB", "A\nB"); ok {
		t.Fatal("a blank row taken into a line")
	}
	// Blank rows that are blank lines: kept.
	if got, ok := unwrapScreen("A\n\nB b\nb", "A\n\nB bb"); !ok || got != "A\n\nB bb" {
		t.Fatalf("blank lines: %v %q", ok, got)
	}
}
