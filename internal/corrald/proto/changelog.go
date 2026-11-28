package proto

import (
	"encoding/json"
	"strings"
)

// ChangelogEntry holds user-facing release notes for one corrald version.
// Keep lines plain and understandable to end users (no internal jargon).
type ChangelogEntry struct {
	Version string   `json:"version"`
	Zh      []string `json:"zh"`
	En      []string `json:"en"`
}

// defaultChangelog is maintained in corrald source and distributed with each
// release, ordered newest-first. Entries without notes are omitted from display.
var defaultChangelog = []ChangelogEntry{
	{
		Version: "0.2.0",
		Zh: []string{
			"向智能体发送消息和回答审批卡片改由辅助进程执行，响应更快。",
			"智能体对话和状态变化实时推送到页面，减少等待与重复刷新。",
		},
		En: []string{
			"Sending messages and answering approval cards now run through the helper daemon for faster responses.",
			"Agent conversation and status changes push to the page in real time, reducing wait time and repeated refreshes.",
		},
	},
}

// Changelog returns a defensive copy of the built-in corrald changelog
// (ordered newest-first).
func Changelog() []ChangelogEntry {
	out := make([]ChangelogEntry, len(defaultChangelog))
	for i, e := range defaultChangelog {
		out[i] = ChangelogEntry{
			Version: e.Version,
			Zh:      append([]string(nil), e.Zh...),
			En:      append([]string(nil), e.En...),
		}
	}
	return out
}

// MarshalChangelog encodes a changelog slice as JSON.
func MarshalChangelog(entries []ChangelogEntry) ([]byte, error) {
	if entries == nil {
		entries = []ChangelogEntry{}
	}
	return json.Marshal(entries)
}

// NormalizeVersion strips a leading "v" and any git-describe suffix
// (e.g. "v0.2.0-5-g1f9a7f0-dirty" -> "0.2.0") for changelog version matching.
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if idx := strings.IndexByte(v, '-'); idx > 0 {
		// Keep base version before git-describe or pre-release suffix.
		base := v[:idx]
		if strings.ContainsRune(base, '.') {
			v = base
		}
	}
	return v
}

func versionMatches(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return NormalizeVersion(a) == NormalizeVersion(b)
}

func hasNotes(e ChangelogEntry) bool {
	for _, s := range e.Zh {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	for _, s := range e.En {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// FilterChangelog selects the entries to display for a user opening the
// changelog at currentVersion given the last version they previously viewed
// (lastSeenVersion, empty if never viewed).
//
// Rules:
//   - entries is ordered newest-first.
//   - Versions without any notes are never emitted (no empty entries).
//   - When currentVersion matches an entry in entries, entries newer than
//     currentVersion are excluded; if currentVersion is not found (e.g. "dev"
//     or a version with no notes), all available entries up to the latest
//     documented version are eligible without fabricating an empty entry.
//   - When lastSeenVersion matches an entry:
//   - all eligible entries strictly newer than lastSeenVersion are included;
//   - if currentVersion itself has notes, it is always included even if
//     lastSeenVersion == currentVersion (so reopening from host settings
//     still shows the current version's notes).
//   - if currentVersion has no entry in entries (e.g. "dev") and
//     lastSeenVersion matches the latest entry, that latest entry is shown
//     so the user still sees the current version's notes when reopening.
func FilterChangelog(entries []ChangelogEntry, currentVersion, lastSeenVersion string) []ChangelogEntry {
	if len(entries) == 0 {
		return []ChangelogEntry{}
	}
	// Locate currentVersion index if present.
	startIdx := 0
	currentIdx := -1
	if strings.TrimSpace(currentVersion) != "" {
		for i, e := range entries {
			if versionMatches(e.Version, currentVersion) {
				currentIdx = i
				startIdx = i
				break
			}
		}
	}

	// Locate lastSeenVersion index if present.
	seenIdx := -1
	if strings.TrimSpace(lastSeenVersion) != "" {
		for i, e := range entries {
			if versionMatches(e.Version, lastSeenVersion) {
				seenIdx = i
				break
			}
		}
	}

	// If neither currentVersion nor lastSeenVersion matched an entry (e.g. an
	// undocumented build version reopened after already viewing it), show only
	// the newest entry that has notes instead of replaying old history.
	if currentIdx == -1 && seenIdx == -1 && versionMatches(currentVersion, lastSeenVersion) {
		for _, e := range entries {
			if hasNotes(e) {
				return []ChangelogEntry{cleanChangelogEntry(e)}
			}
		}
		return []ChangelogEntry{}
	}

	var out []ChangelogEntry
	for i := startIdx; i < len(entries); i++ {
		if seenIdx >= 0 && i >= seenIdx {
			// Include the current version itself if the user already saw it and
			// explicitly reopened changelog.
			if i == startIdx && (currentIdx == i || (currentIdx == -1 && i == 0 && versionMatches(entries[i].Version, lastSeenVersion))) {
				if hasNotes(entries[i]) {
					out = append(out, cleanChangelogEntry(entries[i]))
				}
			}
			break
		}
		if hasNotes(entries[i]) {
			out = append(out, cleanChangelogEntry(entries[i]))
		}
	}
	if out == nil {
		return []ChangelogEntry{}
	}
	return out
}

func cleanChangelogEntry(e ChangelogEntry) ChangelogEntry {
	clean := func(xs []string) []string {
		var r []string
		for _, s := range xs {
			if t := strings.TrimSpace(s); t != "" {
				r = append(r, t)
			}
		}
		return r
	}
	return ChangelogEntry{
		Version: strings.TrimSpace(e.Version),
		Zh:      clean(e.Zh),
		En:      clean(e.En),
	}
}
