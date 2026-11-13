package convo

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

var externalCall = regexp.MustCompile(`^\[external agent tool (call|result): ([A-Za-z0-9_.:+-]{1,128})\]\s*\n`)

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
	m := externalCall.FindStringSubmatchIndex(text)
	if m == nil {
		return []Part{textPart(KindText, text)}
	}
	kind, name := text[m[2]:m[3]], text[m[4]:m[5]]
	body := strings.TrimSpace(text[m[1]:])
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
