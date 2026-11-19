package auth

import (
	"strings"
	"testing"
)

func TestSuggestUsername(t *testing.T) {
	for _, c := range []struct{ pref, email, sub, want string }{
		{"alice", "", "s", "alice"},
		{"Alice Smith", "", "s", "Alice-Smith"},
		{"", "bob.b+x@example.com", "s", "bob.b-x"},
		{"..x", "", "s", ""},
		{"张三", "", "s", ""},
		{"a", "", "s", ""},
		{strings.Repeat("z", 60), "", "s", strings.Repeat("z", 26)},
	} {
		got := SuggestUsername(c.pref, c.email, c.sub)
		if c.want == "" {
			if !strings.HasPrefix(got, "sso-") || CheckUsername(got) != nil {
				t.Errorf("%q: %q", c.pref, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%q/%q: %q", c.pref, c.email, got)
		}
	}
}

func TestClaimAt(t *testing.T) {
	c := map[string]any{"groups": []any{"a", "b", 3}, "one": "x", "realm_access": map[string]any{"roles": []any{"r1"}}}
	if g, ok := claimAt(c, "groups"); !ok || len(g) != 2 {
		t.Errorf("groups: %v", g)
	}
	if g, _ := claimAt(c, "one"); len(g) != 1 || g[0] != "x" {
		t.Errorf("one: %v", g)
	}
	if g, _ := claimAt(c, "realm_access.roles"); len(g) != 1 || g[0] != "r1" {
		t.Errorf("path: %v", g)
	}
	if _, ok := claimAt(c, "missing"); ok {
		t.Errorf("missing found")
	}
}
