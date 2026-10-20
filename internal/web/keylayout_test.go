package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKeyLayoutSetting(t *testing.T) {
	ok := []string{
		`{"pinned":[{"k":"esc"},{"k":"tab"},{"k":"ctrl"},{"k":"alt"}],"row":[{"k":"fn"},{"label":"新窗","keys":"C-b c","long":"C-b n"},{"k":"up"},{"k":"sym:\\"}]}`,
		`{"pinned":[],"row":[]}`,
		`{"pinned":[{"k":"esc","long":""}],"row":[{"k":"panel"},{"label":"x","keys":"text:/clear, enter"}]}`,
	}
	for _, s := range ok {
		if !validSetting("keyLayout", json.RawMessage(s)) {
			t.Errorf("should be valid: %s", s)
		}
	}
	many := func(n int, item string) string { return strings.TrimSuffix(strings.Repeat(item+",", n), ",") }
	bad := []string{
		`null`, `[]`, `{"pinned":[]}`, `{"row":[]}`,
		`{"pinned":[{"k":"esc"}],"row":[{"k":"esc"}]}`,                                // twice
		`{"pinned":[],"row":[{"k":"nope"}]}`,                                          // unknown
		`{"pinned":[],"row":[{"k":"esc","label":"x"}]}`,                               // both kinds
		`{"pinned":[],"row":[{"label":"x"}]}`,                                         // no keys
		`{"pinned":[],"row":[{"label":"  ","keys":"a"}]}`,                             // blank label
		`{"pinned":[],"row":[{"label":"1234567890123","keys":"a"}]}`,                  // label too long
		`{"pinned":[],"row":[{"label":"x","keys":"a\u0007"}]}`,                        // control character
		`{"pinned":[],"row":[{"k":"esc","long":"` + strings.Repeat("a", 201) + `"}]}`, // long press too long
		`{"pinned":[],"row":[{"k":"esc","color":"red"}]}`,                             // unknown field
		`{"pinned":[],"row":[{"k":"esc","long":null}]}`,                               // null
		`{"pinned":[],"row":[{"K":"esc"}]}`,                                           // another spelling
		`{"pinned":[],"row":[{"k":1}]}`,                                               // not a string
		`{"pinned":[],"row":[],"extra":[]}`,                                           // unknown group
		`{"pinned":[` + many(7, `{"label":"x","keys":"a"}`) + `],"row":[]}`,           // too many pinned
		`{"pinned":[],"row":[` + many(25, `{"label":"x","keys":"a"}`) + `]}`,          // too many own keys
		`{"pinned":[],"row":[` + many(49, `{"k":"up"}`) + `]}`,                        // too many in the row (and repeated)
	}
	for _, s := range bad {
		if validSetting("keyLayout", json.RawMessage(s)) {
			t.Errorf("should be refused: %.120s", s)
		}
	}
}
