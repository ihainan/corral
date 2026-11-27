package askhook

import "testing"

func TestParseVersion(t *testing.T) {
	good := []struct {
		in   string
		want [3]int
	}{
		{"2.1.316", [3]int{2, 1, 316}},
		{"2.1.330 (Claude Code)", [3]int{2, 1, 330}},
		{"  2.1.318 (Claude Code)\n", [3]int{2, 1, 318}},
		{"0.0.0", [3]int{0, 0, 0}},
		{"10.20.30", [3]int{10, 20, 30}},
	}
	for _, c := range good {
		got, ok := ParseVersion(c.in)
		if !ok || got != c.want {
			t.Errorf("ParseVersion(%q) = (%v, %v), want (%v, true)", c.in, got, ok, c.want)
		}
	}

	bad := []string{
		"",
		"   ",
		"unknown",
		"2.1",
		"2",
		"2.1.316.0",
		"v2.1.316",
		"2.1.x",
		"2.1.316-rc.1",
		"2.1.316+build",
		"-2.1.316",
		"+2.1.316",
		"2.-1.316",
		"2.1.316 extra words",
		"error 2.1.316",
		"2.1.316\n2.1.318",
		"99999999999999999999999999.1.1",
	}
	for _, in := range bad {
		if got, ok := ParseVersion(in); ok {
			t.Errorf("ParseVersion(%q) = (%v, true), want (_, false)", in, got)
		}
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b [3]int
		want int
	}{
		{[3]int{2, 1, 315}, [3]int{2, 1, 316}, -1},
		{[3]int{2, 1, 316}, [3]int{2, 1, 316}, 0},
		{[3]int{2, 1, 317}, [3]int{2, 1, 316}, 1},
		{[3]int{2, 0, 999}, [3]int{2, 1, 0}, -1},
		{[3]int{2, 2, 0}, [3]int{2, 1, 999}, 1},
		{[3]int{1, 99, 99}, [3]int{2, 0, 0}, -1},
		{[3]int{3, 0, 0}, [3]int{2, 99, 99}, 1},
	}
	for _, c := range cases {
		if got := CompareVersion(c.a, c.b); got != c.want {
			t.Errorf("CompareVersion(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNeedsHook(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		// Below threshold: keep hook.
		{"2.1.252", true},
		{"2.1.281", true},
		{"2.1.284", true},
		{"2.1.299", true},
		{"2.1.304", true},
		{"2.1.315", true},
		{"2.1.315 (Claude Code)", true},
		{"2.0.999", true},
		{"1.9.999", true},
		// At threshold: no hook needed.
		{NoHookMinVersion, false},
		{"2.1.316", false},
		{"2.1.316 (Claude Code)", false},
		// Above threshold: no hook needed.
		{"2.1.317", false},
		{"2.1.318", false},
		{"2.1.330", false},
		{"2.1.330 (Claude Code)", false},
		{"2.2.0", false},
		{"3.0.0", false},
		// Unknown or malformed: safe fallback keeps hook.
		{"", true},
		{"   ", true},
		{"unknown", true},
		{"2.1", true},
		{"2.1.x", true},
		{"v2.1.316", true},
		{"2.1.316-beta.1", true},
		{"2.1.316.1", true},
		{"2.1.316 garbage", true},
	}
	for _, c := range cases {
		if got := NeedsHook(c.version); got != c.want {
			t.Errorf("NeedsHook(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestHostHookUnnecessary(t *testing.T) {
	cases := []struct {
		name     string
		versions []string
		want     bool
	}{
		{"none detected (nil)", nil, false},
		{"none detected (empty)", []string{}, false},
		{"single below", []string{"2.1.315"}, false},
		{"single equal", []string{"2.1.316"}, true},
		{"single above", []string{"2.1.330 (Claude Code)"}, true},
		{"all at or above", []string{"2.1.316", "2.1.318", "2.1.330 (Claude Code)"}, true},
		{"mixed new binary and old running session", []string{"2.1.330 (Claude Code)", "2.1.304"}, false},
		{"mixed old binary and new session", []string{"2.1.315 (Claude Code)", "2.1.330"}, false},
		{"mixed with unknown", []string{"2.1.330", ""}, false},
		{"mixed with malformed", []string{"2.1.330", "2.1.x"}, false},
	}
	for _, c := range cases {
		if got := HostHookUnnecessary(c.versions); got != c.want {
			t.Errorf("%s (%v): got %v, want %v", c.name, c.versions, got, c.want)
		}
	}
}
