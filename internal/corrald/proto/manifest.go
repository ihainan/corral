package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// ManifestSchema is the current schema version of Manifest.
const ManifestSchema = 1

// Declared capability category constants. Stage 1 agent readers (T3) reference
// these constants so the declared manifest stays in sync with host reads.
const (
	ReadProc                   = "proc"
	ReadAgentLogsClaude        = "agent-logs:claude"
	ReadAgentLogsCodex         = "agent-logs:codex"
	ReadAgentLogsOpenCode      = "agent-logs:opencode"
	ReadAgentLogsDevin         = "agent-logs:devin"
	ReadAgentLogsAgy           = "agent-logs:agy"
	ReadPaneScreenHerdr        = "pane-screen:herdr"
	ReadPaneScreenTmux         = "pane-screen:tmux"
	ReadEnvClaudeConfigDir     = "env:CLAUDE_CONFIG_DIR"
	ReadEnvClaudeCodeSessionID = "env:CLAUDE_CODE_SESSION_ID"
	ReadEnvCodexHome           = "env:CODEX_HOME"

	ActionPaneInput  = "pane-input"
	ActionAnswerCard = "answer-card"

	WriteSelf = "self"

	EgressGateway = "gateway"
)

// Budget declares the upper resource envelope promised by a corrald build.
type Budget struct {
	RSSMB  int `json:"rss_mb"`
	DiskMB int `json:"disk_mb"`
	LogMB  int `json:"log_mb"`
}

// Manifest declares what a corrald binary reads, acts on, writes, where it
// sends data, its resource budget, and optional capabilities.
type Manifest struct {
	Schema   int      `json:"schema"`
	Reads    []string `json:"reads"`
	Actions  []string `json:"actions"`
	Writes   []string `json:"writes"`
	Egress   []string `json:"egress"`
	Budget   Budget   `json:"budget"`
	Optional []string `json:"optional"`
}

// DefaultManifest returns the stage 1 capability declaration compiled into
// corrald. Returned slices are freshly allocated copies.
func DefaultManifest() Manifest {
	return Manifest{
		Schema: ManifestSchema,
		Reads: []string{
			ReadProc,
			ReadAgentLogsClaude,
			ReadAgentLogsCodex,
			ReadAgentLogsOpenCode,
			ReadAgentLogsDevin,
			ReadAgentLogsAgy,
			ReadPaneScreenHerdr,
			ReadPaneScreenTmux,
			ReadEnvClaudeConfigDir,
			ReadEnvClaudeCodeSessionID,
			ReadEnvCodexHome,
		},
		Actions: []string{
			ActionPaneInput,
			ActionAnswerCard,
		},
		Writes: []string{
			WriteSelf,
		},
		Egress: []string{
			EgressGateway,
		},
		Budget: Budget{
			RSSMB:  50,
			DiskMB: 30,
			LogMB:  10,
		},
		Optional: []string{},
	}
}

// MarshalManifest encodes m as canonical UTF-8 JSON with non-nil slices so the
// output is deterministic across runs.
func MarshalManifest(m Manifest) ([]byte, error) {
	norm := Manifest{
		Schema:   m.Schema,
		Reads:    nonNilSlice(m.Reads),
		Actions:  nonNilSlice(m.Actions),
		Writes:   nonNilSlice(m.Writes),
		Egress:   nonNilSlice(m.Egress),
		Budget:   m.Budget,
		Optional: nonNilSlice(m.Optional),
	}
	return json.Marshal(norm)
}

// ManifestSHA256 returns the lowercase hex SHA-256 digest of the canonical JSON
// encoding of m.
func ManifestSHA256(m Manifest) (string, error) {
	b, err := MarshalManifest(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ScopeGrew compares old and next manifests and returns every item where the
// newer release expands privileges, egress, resource budget, optional
// capabilities, or manifest schema.
//
// An empty return slice means the update stays within the previously consented
// scope and may proceed automatically. Ordering changes and scope reductions
// are not treated as growth.
func ScopeGrew(old, next Manifest) []string {
	var added []string

	if old.Schema > 0 && next.Schema > old.Schema {
		added = append(added, fmt.Sprintf("schema:%d", next.Schema))
	}
	added = append(added, diffAdded("reads", old.Reads, next.Reads)...)
	added = append(added, diffAdded("actions", old.Actions, next.Actions)...)
	added = append(added, diffAdded("writes", old.Writes, next.Writes)...)
	added = append(added, diffAdded("egress", old.Egress, next.Egress)...)

	if next.Budget.RSSMB > old.Budget.RSSMB {
		added = append(added, "budget:rss_mb")
	}
	if next.Budget.DiskMB > old.Budget.DiskMB {
		added = append(added, "budget:disk_mb")
	}
	if next.Budget.LogMB > old.Budget.LogMB {
		added = append(added, "budget:log_mb")
	}

	added = append(added, diffAdded("optional", old.Optional, next.Optional)...)
	return added
}

func diffAdded(prefix string, oldItems, nextItems []string) []string {
	existing := make(map[string]struct{}, len(oldItems))
	for _, item := range oldItems {
		existing[item] = struct{}{}
	}
	seen := make(map[string]struct{})
	var out []string
	for _, item := range nextItems {
		if _, ok := existing[item]; ok {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, prefix+":"+item)
	}
	sort.Strings(out)
	return out
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	cp := make([]string, len(s))
	copy(cp, s)
	return cp
}
