// Package askhook keeps the hook that makes Claude Code write a question it
// asks (AskUserQuestion) to its session log as it asks it.
//
// Claude Code (2.1.31x) holds such a question back from its log until it is
// answered — with the full-screen interface in every permission mode, else in
// auto and plan mode (anthropics/claude-code#96795) — so the chat view, drawn
// from the log, has only the screen to go by. A PreToolUse hook matching
// AskUserQuestion, one that does nothing, has it written at once, a running
// Claude Code taking it up from its settings as they change. Only such a
// hook does: a PermissionRequest one, or one for another tool, does not.
//
// The hook is given inline (--settings) to the Claude Code the gateway starts
// itself, and written to the person's settings file only when they ask
// (internal/web/askhook.go). Here: what that file says of it, and the file
// with it put in or taken out, all else as it was (keys in their order).
package askhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// Matcher and Command make the hook; the one element of a PreToolUse entry
// whose matcher is exactly Matcher and whose command is exactly Command is
// the gateway's, and only it is ever taken out.
const (
	Matcher = "AskUserQuestion"
	Command = "true # corral: AskUserQuestion to the log at once"
)

// Inline is the hook as Claude Code's --settings takes it inline.
func Inline() string {
	return `{"hooks":{"PreToolUse":[` + entry() + `]}}`
}

func entry() string {
	c, _ := json.Marshal(Command)
	return `{"matcher":"` + Matcher + `","hooks":[{"type":"command","command":` + string(c) + `}]}`
}

// State is what a settings file says of the hook.
type State string

const (
	On      State = "on"      // the gateway's hook is there
	OnOwn   State = "on-own"  // a hook of the person's own matches AskUserQuestion (plainly: see ownMatch)
	Off     State = "off"     // neither
	Blocked State = "blocked" // hooks do not run (Why says which setting)
)

// ErrShape: the file is not one this package changes safely (not a JSON
// object, its hooks or PreToolUse not of the shape Claude Code reads).
var ErrShape = errors.New("the settings file is not in a shape that can be changed safely")

// Read says what settings (the file's bytes; nil or blank for none) and
// managed (the system's managed-settings.json, nil if none) make of the
// hook. why names the setting that blocks it.
func Read(settings, managed []byte) (st State, why string, err error) {
	top, err := parseTop(settings)
	if err != nil {
		return "", "", err
	}
	if m, err := parseTop(managed); err == nil {
		if isTrue(m.get("allowManagedHooksOnly")) {
			return Blocked, "allowManagedHooksOnly", nil
		}
		if isTrue(m.get("disableAllHooks")) {
			return Blocked, "disableAllHooks", nil
		}
	}
	if isTrue(top.get("disableAllHooks")) {
		return Blocked, "disableAllHooks", nil
	}
	entries, err := preToolUse(top)
	if err != nil {
		return "", "", err
	}
	own := false
	for _, raw := range entries {
		var e struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		mine := e.Matcher != nil && *e.Matcher == Matcher
		for _, h := range e.Hooks {
			if mine && h.Type == "command" && h.Command == Command {
				return On, "", nil
			}
			if h.Type == "command" && strings.TrimSpace(h.Command) != "" && ownMatch(e.Matcher) {
				own = true
			}
		}
	}
	if own {
		return OnOwn, "", nil
	}
	return Off, "", nil
}

// ownMatch: a matcher that plainly takes in AskUserQuestion — none, empty,
// "*", or a list of plain tool names (split by | or ,) naming it. Claude Code
// matches such lists exactly and anything else as a JavaScript regular
// expression, which is not modelled here: such a matcher does not count
// (and the gateway's hook alongside it does no harm).
func ownMatch(m *string) bool {
	if m == nil {
		return true
	}
	s := strings.TrimSpace(*m)
	if s == "" || s == "*" {
		return true
	}
	found := false
	for _, n := range strings.FieldsFunc(s, func(r rune) bool { return r == '|' || r == ',' }) {
		n = strings.TrimSpace(n)
		if !plainName.MatchString(n) {
			return false
		}
		found = found || n == Matcher
	}
	return found
}

var plainName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Enable is settings with the gateway's hook put in (at the end of
// hooks.PreToolUse); changed is false when it was there already.
func Enable(settings []byte) (out []byte, changed bool, err error) {
	top, err := parseTop(settings)
	if err != nil {
		return nil, false, err
	}
	entries, err := preToolUse(top)
	if err != nil {
		return nil, false, err
	}
	if st, _, _ := Read(settings, nil); st == On {
		return settings, false, nil
	}
	hooks := object{}
	if raw := top.get("hooks"); raw != nil {
		if hooks, err = parseObject(raw); err != nil {
			return nil, false, ErrShape
		}
	}
	entries = append(entries, json.RawMessage(entry()))
	hooks = hooks.set("PreToolUse", array(entries))
	top = top.set("hooks", hooks.bytes())
	return render(top, settings), true, nil
}

// Disable is settings with the gateway's hook taken out, and nothing else:
// another hook in the same entry stays; the entry, PreToolUse and hooks go
// only once that leaves them empty. empty: nothing is left in the file
// ({}), so the file can go as well. changed is false when it was not there.
func Disable(settings []byte) (out []byte, empty, changed bool, err error) {
	top, err := parseTop(settings)
	if err != nil {
		return nil, false, false, err
	}
	entries, err := preToolUse(top)
	if err != nil {
		return nil, false, false, err
	}
	var kept []json.RawMessage
	for _, raw := range entries {
		e, err := parseObject(raw)
		if err != nil || !matcherIs(e.get("matcher"), Matcher) {
			kept = append(kept, raw)
			continue
		}
		var hs []json.RawMessage
		if json.Unmarshal(e.get("hooks"), &hs) != nil {
			kept = append(kept, raw)
			continue
		}
		var left []json.RawMessage
		gone := false
		for _, h := range hs {
			var c struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal(h, &c) == nil && c.Type == "command" && c.Command == Command {
				gone = true
				continue
			}
			left = append(left, h)
		}
		switch {
		case !gone:
			kept = append(kept, raw)
		case len(left) > 0:
			kept = append(kept, json.RawMessage(e.set("hooks", array(left)).bytes()))
		}
		changed = changed || gone
	}
	if !changed {
		return settings, top.empty(), false, nil
	}
	hooks, _ := parseObject(top.get("hooks"))
	if len(kept) == 0 {
		hooks = hooks.del("PreToolUse")
	} else {
		hooks = hooks.set("PreToolUse", array(kept))
	}
	if hooks.empty() {
		top = top.del("hooks")
	} else {
		top = top.set("hooks", hooks.bytes())
	}
	return render(top, settings), top.empty(), true, nil
}

// array: the values as a JSON array, each as it was (json.Marshal would
// escape <, > and & in their strings: a command's 2>&1 turned into \u003e).
func array(vals []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		if json.Compact(&b, v) != nil {
			b.Write(v)
		}
	}
	b.WriteByte(']')
	return b.Bytes()
}

func matcherIs(raw json.RawMessage, want string) bool {
	var s string
	return raw != nil && json.Unmarshal(raw, &s) == nil && s == want
}

func isTrue(raw json.RawMessage) bool {
	var b bool
	return raw != nil && json.Unmarshal(raw, &b) == nil && b
}

// preToolUse: hooks.PreToolUse's entries (none when either is absent).
func preToolUse(top object) ([]json.RawMessage, error) {
	raw := top.get("hooks")
	if raw == nil {
		return nil, nil
	}
	hooks, err := parseObject(raw)
	if err != nil {
		return nil, ErrShape
	}
	p := hooks.get("PreToolUse")
	if p == nil {
		return nil, nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(p, &entries) != nil {
		return nil, ErrShape
	}
	return entries, nil
}

// parseTop: the file's top object; nothing (or blank) is an empty one. A
// byte-order mark is not taken (Claude Code's own reading fails on it).
func parseTop(b []byte) (object, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return object{}, nil
	}
	o, err := parseObject(b)
	if err != nil {
		return nil, ErrShape
	}
	return o, nil
}

// render: the file again, indented by two spaces as Claude Code writes it,
// ending in a newline as the old one did (a new one does).
func render(top object, old []byte) []byte {
	var out bytes.Buffer
	_ = json.Indent(&out, top.bytes(), "", "  ")
	if len(bytes.TrimSpace(old)) == 0 || bytes.HasSuffix(old, []byte("\n")) {
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// object is a JSON object with its keys in their order.
type object []member

type member struct {
	key string
	val json.RawMessage
}

func parseObject(b []byte) (object, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, ErrShape
	}
	o := object{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return nil, ErrShape
		}
		key, ok := k.(string)
		if !ok {
			return nil, ErrShape
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, ErrShape
		}
		o = o.set(key, v)
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return nil, ErrShape
	}
	if len(bytes.TrimSpace(b[d.InputOffset():])) > 0 {
		return nil, ErrShape // (something after the object)
	}
	return o, nil
}

func (o object) get(k string) json.RawMessage {
	for _, m := range o {
		if m.key == k {
			return m.val
		}
	}
	return nil
}

// set replaces k's value where it is (a key given twice: its last), else
// adds it at the end.
func (o object) set(k string, v json.RawMessage) object {
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].key == k {
			o[i].val = v
			return o
		}
	}
	return append(o, member{k, v})
}

func (o object) del(k string) object {
	out := o[:0:0]
	for _, m := range o {
		if m.key != k {
			out = append(out, m)
		}
	}
	return out
}

func (o object) empty() bool { return len(o) == 0 }

func (o object) bytes() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k := bytes.Buffer{}
		e := json.NewEncoder(&k)
		e.SetEscapeHTML(false)
		_ = e.Encode(m.key)
		b.Write(bytes.TrimRight(k.Bytes(), "\n"))
		b.WriteByte(':')
		var v bytes.Buffer
		if json.Compact(&v, m.val) != nil {
			v.Reset()
			v.Write(m.val)
		}
		b.Write(v.Bytes())
	}
	b.WriteByte('}')
	return b.Bytes()
}
