package convo

import (
	"regexp"
	"strings"
)

// What Claude Code's compaction summary says, without what it wraps it in
// for the model (doc UI item 10, 11-24: the chat page showed it as written —
// "This session is being continued…", <system-reminder> blocks listing the
// tools used, "Summary:", the instructions after it — before the summary
// itself). Older summaries carry their working-out in <analysis> and the
// summary in <summary>: the summary is kept.

var (
	summaryLead = regexp.MustCompile(`(?i)^\s*` + claudeContinued + `[^\n]*\n*`)
	// (Put before that, where the conversation had pages others wrote: a note to the model of its own.)
	summaryAuthored = regexp.MustCompile(`(?im)^\s*The summarized conversation included Artifact content written by people other than you[^\n]*\n*`)
	summaryReminder = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>\s*`)
	summaryOwnTags  = regexp.MustCompile(`(?is)<(tools-used-before-summary|artifact-content-authored-by-others)\b[^>]*?(?:/>|>.*?</(?:tools-used-before-summary|artifact-content-authored-by-others)>)\s*`)
	summaryAnalysis = regexp.MustCompile(`(?is)^\s*<analysis>.*?</analysis>\s*`)
	summaryWrapped  = regexp.MustCompile(`(?is)^\s*<summary>(.*?)</summary>`)
	summaryHead     = regexp.MustCompile(`(?i)^\s*Summary:\s*\n`)
	// (What it tells the model after the summary: from the first such line on.)
	// (A section's head at the line's start, "1. Primary Request and Intent:": bold, so the
	// sections are found at a glance among their lists.)
	summarySection = regexp.MustCompile(`(?m)^(\d{1,2})\.\s+([^\n*:]{2,60}?):?[ \t]*$`)
	summaryTail    = regexp.MustCompile(`(?im)^\s*(?:If you need specific details from before compaction|Please continue the conversation from where|Continue the conversation from where it left off|Recent messages are preserved verbatim|The messages after this summary are)`)
)

func compactSummaryText(s string) string {
	t := summaryOwnTags.ReplaceAllString(s, "")
	t = summaryAuthored.ReplaceAllString(t, "")
	t = summaryLead.ReplaceAllString(t, "")
	t = summaryReminder.ReplaceAllString(t, "")
	t = summaryAnalysis.ReplaceAllString(t, "")
	if m := summaryWrapped.FindStringSubmatch(t); m != nil {
		t = m[1]
	}
	t = summaryHead.ReplaceAllString(t, "")
	if loc := summaryTail.FindStringIndex(t); loc != nil && loc[0] > 0 {
		t = t[:loc[0]]
	}
	t = summarySection.ReplaceAllString(t, "$1. **$2**")
	t = strings.TrimSpace(t)
	if t == "" { // (nothing left that is not wrapping: as it was, not an empty block)
		return strings.TrimSpace(s)
	}
	return t
}
