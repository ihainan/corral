package askhook

import (
	"strconv"
	"strings"
)

// NoHookMinVersion is the first Claude Code version known to write
// AskUserQuestion to its session log while waiting for an answer without the
// PreToolUse hook.
//
// Verified 2026-11-27 with live models on Claude Code 2.1.316, 2.1.318 and
// 2.1.330; older versions (first reported on 2.1.281, still observed running
// in production on 2.1.252, 2.1.284 and 2.1.299-2.1.304) still keep the hook.
const NoHookMinVersion = "2.1.316"

var noHookMin = func() [3]int {
	v, ok := ParseVersion(NoHookMinVersion)
	if !ok {
		panic("askhook: invalid NoHookMinVersion: " + NoHookMinVersion)
	}
	return v
}()

// ParseVersion parses a Claude Code version string into (major, minor, patch).
// Accepts either a bare "X.Y.Z" (session log "version" field) or
// "X.Y.Z (Claude Code)" (output of `claude --version`). Returns ok=false for
// empty, unknown, pre-release, or malformed strings.
func ParseVersion(raw string) ([3]int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return [3]int{}, false
	}
	ver, rest, hasSpace := strings.Cut(s, " ")
	if hasSpace {
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
			return [3]int{}, false
		}
	}
	parts := strings.Split(ver, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		if p == "" {
			return [3]int{}, false
		}
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return [3]int{}, false
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// CompareVersion compares two (major, minor, patch) tuples, returning -1, 0, or 1.
func CompareVersion(a, b [3]int) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// NeedsHook reports whether a Claude Code instance of the given version needs
// the AskUserQuestion PreToolUse hook. Versions below NoHookMinVersion, as well
// as unknown or malformed versions, return true so older or unrecognized builds
// keep full question cards.
func NeedsHook(version string) bool {
	v, ok := ParseVersion(version)
	if !ok {
		return true
	}
	return CompareVersion(v, noHookMin) < 0
}

// HostHookUnnecessary reports whether every Claude Code version detected on a
// host is known and at least NoHookMinVersion, meaning an existing hook in
// ~/.claude/settings.json is no longer needed and may be turned off.
// Returns false when no version was detected or any detected version is below
// NoHookMinVersion or unparseable.
func HostHookUnnecessary(versions []string) bool {
	if len(versions) == 0 {
		return false
	}
	for _, v := range versions {
		if NeedsHook(v) {
			return false
		}
	}
	return true
}
