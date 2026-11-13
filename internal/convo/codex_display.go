package convo

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

// A tool's call or result as an import from another agent (Claude Code's,
// by Codex's own import) writes it in an assistant message:
// "[external_agent_tool_call: Bash]\n…\n[/external_agent_tool_call]" and
// "[external_agent_tool_result]\n…" (seen so in 0.164's imports; spaced, by
// the app server's account of them).
var (
	externalCall    = regexp.MustCompile(`^\[external[ _]agent[ _]tool[ _](call|result)(?::\s*([A-Za-z0-9_.:+-]{1,128}))?\]\s*(?:\n|$)`)
	externalCallEnd = regexp.MustCompile(`\n?\[/external[ _]agent[ _]tool[ _]call\]\s*$`)
)

// codexImportMark: the message an import ends with ("<EXTERNAL SESSION
// IMPORTED>"): Codex's own mark, nothing said by anyone.
const codexImportMark = "<EXTERNAL SESSION IMPORTED>"

// claudeContinued: how the summary Claude Code begins a compacted session
// with starts (in its log, a row of its own marked isCompactSummary; in an
// import, the person's message).
const claudeContinued = "This session is being continued from a previous conversation"

// importedRecord: an imported tool's call or result (kind "call" or
// "result"; a result names none), its body; ok false, another message.
func importedRecord(text string) (kind, name, body string, ok bool) {
	m := externalCall.FindStringSubmatchIndex(text)
	if m == nil {
		return "", "", "", false
	}
	kind = text[m[2]:m[3]]
	if m[4] >= 0 {
		name = text[m[4]:m[5]]
	}
	if kind == "call" && name == "" {
		return "", "", "", false
	}
	body = strings.TrimSpace(externalCallEnd.ReplaceAllString(text[m[1]:], ""))
	return kind, name, body, true
}

func codexTaskNotice(text string) *Event {
	text, ok := noticeOf(text)
	if !ok {
		return nil
	}
	if event := taskEnded(text); event != nil {
		return event
	}
	if match := taskSummary.FindStringSubmatch(text); match != nil {
		return &Event{Kind: EventNotice, Level: "notice", Text: clip(strings.TrimSpace(html.UnescapeString(match[1])), maxDetail)}
	}
	return nil
}

// Imported tool records are preserved as inspectable, collapsed tools instead
// of being rendered as a page-long assistant message. Only an exact record
// header at the start of an assistant block is interpreted.
func codexDisplayParts(text string) []Part {
	kind, name, body, ok := importedRecord(text)
	if !ok {
		return []Part{textPart(KindText, text)}
	}
	if name == "" {
		name = "result"
	}
	tool := &Tool{Name: name, Done: true}
	if kind == "result" {
		tool.setResult(body)
	} else {
		if strings.HasPrefix(body, "description: ") {
			line, rest, ok := strings.Cut(body, "\n")
			if ok {
				tool.Title = clip(strings.TrimPrefix(line, "description: "), maxDetail)
				body = rest
			}
		}
		tool.Detail = clip(strings.TrimPrefix(body, "command: "), maxDetail)
		// Keep the full migrated record available when its command was clipped.
		if len(body) > maxDetail {
			tool.setResult(body)
		}
	}
	return []Part{{Kind: KindTool, Tool: tool}}
}

func codexReplyText(text string) (string, bool) {
	const start = "<send_user_message_question_reply>"
	const end = "</send_user_message_question_reply>"
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, start) || !strings.HasSuffix(s, end) {
		return text, false
	}
	var rows []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(s, start), end)), &rows) != nil || len(rows) == 0 {
		return text, false
	}
	var out []string
	for _, row := range rows {
		if strings.TrimSpace(row.Answer) == "" {
			return text, false
		}
		if row.Question != "" {
			out = append(out, row.Question+"\n"+row.Answer)
		} else {
			out = append(out, row.Answer)
		}
	}
	return strings.Join(out, "\n\n"), true
}

// importedUserText: what a person's row of an imported Claude Code session
// says (as classifyUserText), where what Claude Code wrapped in it for its
// own — its reminders, its caveat — is cut out first, and the rest is told
// as an envelope only where it is one whole (a command, its output): the
// person's own words that hold such a tag, or quote one, are theirs.
func importedUserText(s string) (role, text string) {
	s = claudeOwnBlocks.ReplaceAllString(s, "")
	if strings.TrimSpace(s) == "" {
		return "", ""
	}
	if strings.TrimSpace(wholeEnvelope.ReplaceAllString(s, "")) == "" {
		return classifyUserText(strings.TrimSpace(s))
	}
	return RoleUser, strings.TrimSpace(s)
}

// claudeOwnBlocks: what Claude Code writes into the person's rows for itself.
var claudeOwnBlocks = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>|<local-command-caveat>.*?</local-command-caveat>`)

// wholeEnvelope: one tag of Claude Code's and what it holds.
var wholeEnvelope = regexp.MustCompile(`(?s)<([a-z][a-z0-9-]*)>.*?</([a-z][a-z0-9-]*)>`)
