package herdr

import (
	"context"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

// The shortcut panel presses some of herdr's own keys (the ones for its
// prompts, pickers and modes, which the socket API does not open). Users
// rebind them, and the prefix too, in config.toml: the gateway reads its
// [keys] table, so the panel presses what this host's herdr expects.

// KeysCommand prints herdr's config.toml, at most 64 KiB of it (nothing when
// there is none). Fixed text.
const KeysCommand = `sh -c 'f="${HERDR_CONFIG_PATH:-${XDG_CONFIG_HOME:-$HOME/.config}/herdr/config.toml}"; [ -f "$f" ] && head -c 65536 "$f"; exit 0'`

const keysLimit = 64 << 10

// KeyNames are the bindings the panel may press (and the prefix).
var KeyNames = map[string]bool{
	"prefix": true, "help": true, "settings": true, "goto": true, "open_notification_target": true,
	"workspace_picker": true, "rename_workspace": true, "close_workspace": true, "previous_workspace": true,
	"next_workspace": true, "toggle_sidebar": true, "rename_tab": true, "close_tab": true, "switch_tab": true,
	"rename_pane": true, "edit_scrollback": true, "copy_mode": true, "cycle_pane_next": true,
	"cycle_pane_previous": true, "last_pane": true, "resize_mode": true,
}

// A binding as herdr writes them ("prefix+shift+t", "prefix+T", "ctrl+b",
// "prefix+1..9"): printable, no spaces; "" is a binding taken away.
var keyValue = regexp.MustCompile(`^[!-~]{0,40}$`)

// ReadKeys reads the bindings of KeyNames set in the host's config.toml:
// none set (or no file) is an empty map, herdr's defaults then apply.
func ReadKeys(ctx context.Context, c *ssh.Client) (map[string]string, error) {
	out, err := sshx.Output(ctx, c, KeysCommand, keysLimit)
	if err != nil {
		return nil, err
	}
	return ParseKeys(string(out)), nil
}

// ParseKeys takes the [keys] table's values of KeyNames from a config.toml:
// a string, or an array's first string (herdr takes several). It reads only
// that much of TOML. A binding it cannot read (a multi-line array, say) is
// "", left out like one taken away: never herdr's default in its place.
func ParseKeys(toml string) map[string]string {
	out := map[string]string{}
	section := ""
	for _, line := range strings.Split(toml, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' && !strings.HasPrefix(line, "[[") {
			// ([keys], [ "keys" ]; not [keys.indexed] or [[keys.command]].)
			name, _, _ := strings.Cut(line[1:], "]")
			section = strings.Trim(strings.TrimSpace(name), `"'`)
			continue
		}
		if line[0] == '[' {
			section = ""
			continue
		}
		if section != "keys" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		if !KeyNames[k] {
			continue
		}
		out[k] = firstString(strings.TrimSpace(v))
	}
	return out
}

// firstString is the string a value starts with (after an array's "["),
// basic or literal; "" for anything else, an empty array too. herdr's
// bindings never need escapes: one with a backslash is not read either.
func firstString(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(v, "["))
	if v == "" || (v[0] != '"' && v[0] != '\'') {
		return ""
	}
	end := strings.IndexByte(v[1:], v[0])
	if end < 0 {
		return ""
	}
	b := v[1 : 1+end]
	if strings.Contains(b, `\`) || !keyValue.MatchString(b) {
		return ""
	}
	return b
}
