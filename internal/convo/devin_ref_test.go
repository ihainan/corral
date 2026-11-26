package convo

import "testing"

func TestDevinRefs(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"see <ref_file file=\"/w/notes.txt\" />.", "see `/w/notes.txt`."},
		{"<ref_snippet file=\"/w/a.go\" lines=\"3-9\" /> and <ref_snippet file=\"/w/b.go\" lines=\"12\" />", "`/w/a.go:3-9` and `/w/b.go:12`"},
		{"open <ref_snippet file=\"/w/a.go\" lines=\"3-9\">", "open `/w/a.go:3-9`"},
		{"no refs here", "no refs here"},
		{"<ref_file ... />", "<ref_file ... />"},                         // (an example, no file named: left)
		{"<ref_file file=\"/w/a`b\" />", "<ref_file file=\"/w/a`b\" />"}, // (would break the code span: left)
		{"<ref_call_id>x</ref_call_id>", "<ref_call_id>x</ref_call_id>"},
	} {
		if got := dvRefs(c.in); got != c.want {
			t.Errorf("dvRefs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
