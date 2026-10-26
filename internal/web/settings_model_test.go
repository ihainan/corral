package web

import (
	"encoding/json"
	"testing"
)

// The default model put back, the rest of the settings as they are.
func TestWithModel(t *testing.T) {
	base := "{\n  \"permissions\": {\n    \"allow\": [\"Bash(ls)\"]\n  },\n  \"effortLevel\": \"high\",\n  \"modelSettings\": {\n    \"claude-opus-5-5\": {\n      \"effortLevel\": \"high\"\n    }\n  },\n  \"advisorModel\": \"fable\""
	cases := []struct {
		name, after string
		before      string // ("" none)
	}{
		{"changed", base + ",\n  \"model\": \"sonnet\"\n}\n", `"opus"`},
		{"added at the end", base + ",\n  \"model\": \"sonnet\"\n}\n", ""},
		{"added first", "{\n  \"model\": \"sonnet\",\n" + base[2:] + "\n}\n", ""},
		{"taken out", base + "\n}\n", `"opus"`},
		{"as it was", base + ",\n  \"model\": \"opus\"\n}\n", `"opus"`},
	}
	for _, c := range cases {
		out, err := withModel([]byte(c.after), json.RawMessage(c.before), c.before != "")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Errorf("%s: not JSON: %v\n%s", c.name, err, out)
			continue
		}
		got, ok := m["model"]
		if c.before == "" && ok || c.before != "" && string(got) != c.before {
			t.Errorf("%s: model %s %v\n%s", c.name, got, ok, out)
		}
		if string(m["advisorModel"]) != `"fable"` || len(m) != 4+boolInt(c.before != "") {
			t.Errorf("%s: the rest changed: %s", c.name, out)
		}
	}
	// (Not an object: refused, nothing to write.)
	if _, err := withModel([]byte("[1]"), json.RawMessage(`"opus"`), true); err == nil {
		t.Error("an array taken")
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
