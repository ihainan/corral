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
const KeysCommand = `sh -c 'f="${XDG_CONFIG_HOME:-$HOME/.config}/herdr/config.toml"; [ -f "$f" ] && head -c 65536 "$f"; exit 0'`

const keysLimit = 64 << 10

// KeyNames are the bindings the panel may press (and the prefix).
var KeyNames = map[string]bool{
	"prefix": true, "help": true, "settings": true, "goto": true, "open_notification_target": true,
	"workspace_picker": true, "rename_workspace": true, "close_workspace": true, "previous_workspace": true,
	"next_workspace": true, "toggle_sidebar": true, "rename_tab": true, "close_tab": true, "switch_tab": true,
	"rename_pane": true, "edit_scrollback": true, "copy_mode": true, "cycle_pane_next": true,
	"cycle_pane_previous": true, "last_pane": true, "resize_mode": true,
}

// A binding as herdr writes them ("prefix+shift+t", "ctrl+b", "prefix+1..9");
// "" is a binding taken away.
var keyValue = regexp.MustCompile(`^[a-z0-9+.?\-]{0,40}$`)

// ReadKeys reads the bindings of KeyNames set in the host's config.toml:
// none set (or no file) is an empty map, herdr's defaults then apply.
func ReadKeys(ctx context.Context, c *ssh.Client) (map[string]string, error) {
	out, err := sshx.Output(ctx, c, KeysCommand, keysLimit)
	if err != nil {
		return nil, err
	}
	return ParseKeys(string(out)), nil
}

// ParseKeys takes the [keys] table's string values of KeyNames from a
// config.toml (an array's first string, if herdr takes several). It reads
// only that much of TOML; anything else is passed over.
func ParseKeys(toml string) map[string]string {
	out := map[string]string{}
	section := ""
	for _, line := range strings.Split(toml, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			// ([[keys.command]] and [keys.indexed] are other tables.)
			section = ""
			if strings.HasPrefix(line, "[keys]") {
				section = "keys"
			}
			continue
		}
		if section != "keys" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"`)
		if !KeyNames[k] {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.TrimPrefix(v, "[")
		v = strings.TrimSpace(v)
		if !strings.HasPrefix(v, `"`) {
			continue
		}
		end := strings.IndexByte(v[1:], '"')
		if end < 0 {
			continue
		}
		if b := strings.ToLower(v[1 : 1+end]); keyValue.MatchString(b) {
			out[k] = b
		}
	}
	return out
}
