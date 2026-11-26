package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/convo"
)

// Claude Code 2.1.330 Rewind screen parsers and 2-phase key-driving state
// machine. Screen parsing only inspects the region strictly below the last
// ▔ divider line (and rejects if a normal prompt box follows it), so prose in
// the conversation transcript can never spoof a picker or confirm screen.

const rewindPanelFooter = "Enter to continue · Esc to cancel"

var (
	rewindOptLineRe      = regexp.MustCompile(`^\s*(❯)?\s*(\d+)\.\s+(.+?)\s*$`)
	rewindAgoOnlyRe      = regexp.MustCompile(`^\((?:just now|\d+[smhd]\s+ago|[^()]*\bago)\)$`)
	rewindAgoTailRe      = regexp.MustCompile(`\s+\((?:just now|\d+[smhd]\s+ago)\)$`)
	rewindDiffLineRe     = regexp.MustCompile(`^(?:No code changes|\d+\s+files?\s+changed\b|Restores?\b|\+\d+\s+-\d+)`)
	rewindCodeRestoredRe = regexp.MustCompile(`^The code will be restored(?: [+-][0-9]+(?:,[0-9]+)*)* in `)
	rewindCodeManyRe     = regexp.MustCompile(`^(.+) and ([1-9][0-9]*) other files$`)
	rewindCodeOtherRe    = regexp.MustCompile(`^[0-9]+ other files?$`)
)

func isOverlineRule(line string) bool {
	t := strings.TrimSpace(strings.TrimRight(line, "\r"))
	return len([]rune(t)) >= 8 && strings.Trim(t, "▔") == ""
}

func hasNormalInputBox(lines []string) bool {
	for i := len(lines) - 2; i >= 1; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "❯") || !boxTop(lines[i-1]) {
			continue
		}
		top := strings.TrimRight(lines[i-1], " \r")
		for j := i + 1; j < len(lines) && j <= i+inputLinesMost; j++ {
			if boxFoot(top, strings.TrimRight(lines[j], " \r")) {
				return true
			}
		}
	}
	return false
}

func rewindPanelLines(screen string) ([]string, bool) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if isOverlineRule(lines[i]) {
			panel := lines[i+1:]
			if hasNormalInputBox(panel) {
				return nil, false
			}
			return panel, true
		}
	}
	return nil, false
}

type rewindPicker struct {
	selected  string
	hasCursor bool
	onCurrent bool
	items     []string
}

func (p rewindPicker) matchCount(targetText string) int {
	n := 0
	for _, it := range p.items {
		if matchPickerSelection(it, targetText) {
			n++
		}
	}
	return n
}

func isRewindConfirmOptionLabel(label string) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "restore code and conversation",
		"restore conversation",
		"restore code",
		"summarize from here",
		"summarize up to here",
		"never mind":
		return true
	}
	return false
}

func isRewindConfirmOptionLine(raw string) bool {
	m := rewindOptLineRe.FindStringSubmatch(raw)
	return m != nil && isRewindConfirmOptionLabel(m[3])
}

func hasRewindConfirmOptions(panel []string) bool {
	optLines, knownOpts := 0, 0
	for _, raw := range panel {
		if m := rewindOptLineRe.FindStringSubmatch(raw); m != nil {
			optLines++
			if isRewindConfirmOptionLabel(m[3]) {
				knownOpts++
			}
		}
	}
	return optLines >= 2 && knownOpts >= 1
}

// isRewindPanelOpen reports whether a rewind picker or confirm panel appears
// open strictly below the last ▔ divider (with no normal prompt box below it).
// Intentionally lightweight so cleanup never relies on full screen parsing.
func isRewindPanelOpen(screen string) bool {
	panel, ok := rewindPanelLines(screen)
	if !ok {
		return false
	}
	for _, raw := range panel {
		t := strings.TrimSpace(raw)
		if t == "Rewind" || t == rewindPanelFooter || isRewindConfirmOptionLine(raw) {
			return true
		}
	}
	return false
}

func parseRewindPicker(screen string) (rewindPicker, bool) {
	panel, ok := rewindPanelLines(screen)
	if !ok {
		return rewindPicker{}, false
	}
	if hasRewindConfirmOptions(panel) {
		return rewindPicker{}, false
	}
	var hasTitle, hasHint bool
	startIdx := -1
	footerIdx := -1
	for i, raw := range panel {
		t := strings.TrimSpace(raw)
		if !hasTitle && t == "Rewind" {
			hasTitle, startIdx = true, i+1
		}
		if strings.Contains(t, "Restore the code and/or conversation") || t == rewindPanelFooter {
			hasHint = true
		}
		if t == rewindPanelFooter {
			footerIdx = i
		}
	}
	if !hasTitle || !hasHint || startIdx < 0 {
		return rewindPicker{}, false
	}
	endIdx := len(panel)
	if footerIdx >= startIdx {
		endIdx = footerIdx
	}
	var pk rewindPicker
	for _, raw := range panel[startIdx:endIdx] {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "Restore the code and/or conversation") {
			continue
		}
		if strings.HasPrefix(t, "❯") {
			val := strings.TrimSpace(strings.TrimPrefix(t, "❯"))
			pk.hasCursor, pk.selected = true, val
			if val == "(current)" {
				pk.onCurrent = true
			} else if val != "" {
				pk.items = append(pk.items, val)
			}
			continue
		}
		if t == "(current)" || rewindDiffLineRe.MatchString(t) {
			continue
		}
		pk.items = append(pk.items, t)
	}
	return pk, pk.hasCursor
}

type rewindOption struct {
	num      int
	label    string
	selected bool
}

type rewindCodeDesc struct {
	kind  string   // "one", "two", or "many"
	files []string // 1 name for "one", 2 names for "two", [firstName] for "many"
	total int      // 1 for "one", 2 for "two", N+1 for "many"
}

type rewindConfirm struct {
	quoted     []string
	options    []rewindOption
	codeLine   string
	codeBlocks int
	codeDesc   rewindCodeDesc
	codeDescOK bool
}

func hasRewindEllipsis(s string) bool {
	return strings.HasSuffix(s, "…") || strings.HasSuffix(s, "...")
}

// parseRewindCodeFiles parses Claude Code 2.1.330's confirmation description
// line into its exact shape:
//   - 1 file:   "The code will be restored +1 -2 in a.txt."
//   - 2 files:  "The code will be restored +1 -5 in a.txt and b.txt."
//   - 3+ files: "The code will be restored -4 in a.txt and 3 other files."
//
// Any other shape (truncated ellipsis, "in N files.", comma-separated lists
// without "and N other files", etc.) returns false or fails count matching so
// callers fail closed.
func parseRewindCodeFiles(line string) (rewindCodeDesc, bool) {
	loc := rewindCodeRestoredRe.FindStringIndex(line)
	if loc == nil {
		return rewindCodeDesc{}, false
	}
	s := strings.TrimSpace(line[loc[1]:])
	if s == "" || hasRewindEllipsis(s) {
		return rewindCodeDesc{}, false
	}
	if strings.HasSuffix(s, ".") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "."))
	}
	if s == "" || hasRewindEllipsis(s) || strings.Contains(s, "…") || strings.Contains(s, "...") {
		return rewindCodeDesc{}, false
	}
	if m := rewindCodeManyRe.FindStringSubmatch(s); m != nil {
		name := strings.TrimSpace(m[1])
		otherN, err := strconv.Atoi(m[2])
		if err != nil || name == "" || otherN < 2 {
			return rewindCodeDesc{}, false
		}
		return rewindCodeDesc{kind: "many", files: []string{name}, total: otherN + 1}, true
	}
	if pos := strings.Index(s, " and "); pos >= 0 {
		left := strings.TrimSpace(s[:pos])
		right := strings.TrimSpace(s[pos+5:])
		if left == "" || right == "" || strings.Contains(right, " and ") || rewindCodeOtherRe.MatchString(right) {
			return rewindCodeDesc{}, false
		}
		return rewindCodeDesc{kind: "two", files: []string{left, right}, total: 2}, true
	}
	return rewindCodeDesc{kind: "one", files: []string{s}, total: 1}, true
}

func matchRewindCodeFiles(desc rewindCodeDesc, descOK bool, expectedPaths []string) bool {
	if !descOK || len(expectedPaths) == 0 {
		return false
	}
	expBases, _, err := normalizeRewindFiles(expectedPaths)
	if err != nil || len(expBases) == 0 || desc.total != len(expBases) {
		return false
	}
	switch len(expBases) {
	case 1:
		return desc.kind == "one" && len(desc.files) == 1 && desc.files[0] == expBases[0]
	case 2:
		return desc.kind == "two" && len(desc.files) == 2 &&
			((desc.files[0] == expBases[0] && desc.files[1] == expBases[1]) ||
				(desc.files[0] == expBases[1] && desc.files[1] == expBases[0]))
	default:
		return desc.kind == "many" && len(desc.files) == 1 && slices.Contains(expBases, desc.files[0])
	}
}

func (c rewindConfirm) selectedOption() (rewindOption, bool) {
	for _, o := range c.options {
		if o.selected {
			return o, true
		}
	}
	return rewindOption{}, false
}

func parseRewindConfirm(screen string) (rewindConfirm, bool) {
	panel, ok := rewindPanelLines(screen)
	if !ok {
		return rewindConfirm{}, false
	}
	hdr := -1
	footerIdx := -1
	for i, raw := range panel {
		t := strings.TrimSpace(raw)
		if hdr < 0 && strings.Contains(t, "Restore to the point before you sent this message:") {
			hdr = i
		}
		if t == rewindPanelFooter {
			footerIdx = i
		}
	}
	startIdx := 0
	if hdr >= 0 {
		startIdx = hdr + 1
	} else if !hasRewindConfirmOptions(panel) {
		return rewindConfirm{}, false
	}
	endIdx := len(panel)
	if footerIdx >= startIdx {
		endIdx = footerIdx
	}
	var cf rewindConfirm
	inOpts, selectedCount, knownOpts := false, 0, 0
	inCode := 0
	for _, raw := range panel[startIdx:endIdx] {
		t := strings.TrimSpace(raw)
		if t == "" {
			if inCode == 1 {
				inCode = 2
			}
			continue
		}
		if m := rewindOptLineRe.FindStringSubmatch(raw); m != nil {
			inOpts = true
			if inCode == 1 {
				inCode = 2
			}
			n, _ := strconv.Atoi(m[2])
			sel := m[1] == "❯"
			if sel {
				selectedCount++
			}
			label := strings.TrimSpace(m[3])
			if isRewindConfirmOptionLabel(label) {
				knownOpts++
			}
			cf.options = append(cf.options, rewindOption{num: n, label: label, selected: sel})
			continue
		}
		if !inOpts && strings.HasPrefix(t, "│") {
			if inCode == 1 {
				inCode = 2
			}
			if body := strings.TrimSpace(strings.TrimPrefix(t, "│")); body != "" {
				cf.quoted = append(cf.quoted, body)
			}
			continue
		}
		if !inOpts {
			os := oneSpaced(raw)
			if os == "The code will be restored" || strings.HasPrefix(os, "The code will be restored ") {
				cf.codeBlocks++
				cf.codeLine = os
				inCode = 1
			} else if inCode == 1 {
				if strings.HasPrefix(os, "The conversation ") || strings.HasPrefix(os, "The code ") || strings.HasPrefix(os, "⚠") {
					inCode = 2
				} else {
					cf.codeLine += " " + os
				}
			}
		}
	}
	if cf.codeBlocks == 1 {
		cf.codeDesc, cf.codeDescOK = parseRewindCodeFiles(cf.codeLine)
	}
	if len(cf.quoted) > 1 && rewindAgoOnlyRe.MatchString(cf.quoted[len(cf.quoted)-1]) {
		cf.quoted = cf.quoted[:len(cf.quoted)-1]
	} else if len(cf.quoted) > 0 {
		if trimmed := rewindAgoTailRe.ReplaceAllString(cf.quoted[len(cf.quoted)-1], ""); trimmed != "" {
			cf.quoted[len(cf.quoted)-1] = trimmed
		}
	}
	if len(cf.options) < 2 || selectedCount != 1 || knownOpts < 1 {
		return rewindConfirm{}, false
	}
	return cf, true
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func matchEllipsisPrefix(got, wantFirst, wantAll string) bool {
	if !strings.HasSuffix(got, "…") && !strings.HasSuffix(got, "...") {
		return false
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(got, "…"), "..."))
	if len([]rune(prefix)) < min(4, len([]rune(wantFirst))) {
		return false
	}
	if strings.HasPrefix(wantFirst, prefix) || strings.HasPrefix(wantAll, prefix) {
		return true
	}
	sqPref, sqFirst, sqAll := squeeze(prefix), squeeze(wantFirst), squeeze(wantAll)
	return len([]rune(sqPref)) >= min(4, len([]rune(sqFirst))) && (strings.HasPrefix(sqFirst, sqPref) || strings.HasPrefix(sqAll, sqPref))
}

func matchPickerSelection(selected, targetText string) bool {
	got, wantFirst, wantAll := oneSpaced(selected), oneSpaced(firstNonEmptyLine(targetText)), oneSpaced(targetText)
	if got == "" || wantFirst == "" {
		return false
	}
	if strings.HasSuffix(got, "…") || strings.HasSuffix(got, "...") {
		return matchEllipsisPrefix(got, wantFirst, wantAll)
	}
	return got == wantFirst || got == wantAll
}

func matchConfirmQuote(quoted []string, targetText string) bool {
	got, wantFirst, wantAll := oneSpaced(strings.Join(quoted, " ")), oneSpaced(firstNonEmptyLine(targetText)), oneSpaced(targetText)
	if got == "" || wantAll == "" {
		return false
	}
	if strings.HasSuffix(got, "…") || strings.HasSuffix(got, "...") {
		return matchEllipsisPrefix(got, wantFirst, wantAll)
	}
	if got == wantAll || got == wantFirst {
		return true
	}
	if len(quoted) > 1 {
		sqGot := squeeze(got)
		if sqGot == squeeze(wantAll) || sqGot == squeeze(wantFirst) {
			return true
		}
	}
	return false
}

func screenAboveAndBox(screen string) (above, box string, hasBox bool) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n")
	for i := len(lines) - 2; i >= 1; i-- {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "❯") || !boxTop(lines[i-1]) {
			continue
		}
		top := strings.TrimRight(lines[i-1], " \r")
		for j := i + 1; j < len(lines) && j <= i+inputLinesMost; j++ {
			if boxFoot(top, strings.TrimRight(lines[j], " \r")) {
				parts := []string{strings.TrimSpace(strings.TrimPrefix(t, "❯"))}
				for k := i + 1; k < j; k++ {
					parts = append(parts, strings.TrimSpace(lines[k]))
				}
				return strings.Join(lines[:i-1], "\n"), oneSpaced(strings.Join(parts, " ")), true
			}
		}
	}
	return "", "", false
}

func matchInputRefill(boxText, targetText string) bool {
	got, wantFirst, wantAll := oneSpaced(boxText), oneSpaced(firstNonEmptyLine(targetText)), oneSpaced(targetText)
	if got == "" || wantAll == "" {
		return false
	}
	if got == wantAll || got == wantFirst {
		return true
	}
	sqGot, sqWant := squeeze(got), squeeze(wantAll)
	if sqGot == sqWant || (typedPasted.MatchString(sqGot) && strings.Contains(strings.TrimRight(targetText, "\n"), "\n")) {
		return true
	}
	return len([]rune(sqGot)) >= min(8, len([]rune(sqWant))) && strings.HasPrefix(sqWant, sqGot)
}

type claudeRewindPlan struct {
	Steps             int
	TargetText        string
	Ambiguous         bool
	WithdrawnSnippets []string
	RestoreCode       bool
	ExpectedFiles     []string
}

func entryUserText(e convo.Entry) (text string, truncated bool) {
	var parts []string
	for _, p := range e.Parts {
		if p.Kind == convo.KindText && p.Text != "" {
			parts = append(parts, p.Text)
			if p.Truncated {
				truncated = true
			}
		}
	}
	return strings.Join(parts, "\n"), truncated
}

func hasActiveRewindTarget(entries []convo.Entry, targetID string) bool {
	for _, e := range entries {
		if e.Role == convo.RoleUser && !e.Gone && !e.Mid && e.ID != "" && e.ID == targetID {
			return true
		}
	}
	return false
}

func buildClaudeRewindPlan(entries []convo.Entry, targetID, fullText string) (claudeRewindPlan, *rewindError) {
	type activeUser struct {
		idx         int
		text, first string
		truncated   bool
	}
	var users []activeUser
	targetIdx := -1
	for i, e := range entries {
		if e.Role != convo.RoleUser || e.Gone || e.Mid || e.ID == "" {
			continue
		}
		txt, trunc := entryUserText(e)
		if e.ID == targetID {
			targetIdx = len(users)
		}
		users = append(users, activeUser{idx: i, text: txt, first: oneSpaced(firstNonEmptyLine(txt)), truncated: trunc})
	}
	if targetIdx < 0 {
		return claudeRewindPlan{}, rwErr(http.StatusConflict, "message_not_found", "that message is no longer in the active conversation")
	}
	target := users[targetIdx]
	if fullText != "" {
		if len(fullText) > maxSendText {
			return claudeRewindPlan{}, rwErr(http.StatusConflict, "message_too_long", "message exceeds the 100 KiB composer limit and cannot be restored completely")
		}
		target.text = fullText
		target.first = oneSpaced(firstNonEmptyLine(fullText))
		target.truncated = false
	} else {
		if target.truncated {
			return claudeRewindPlan{}, rwErr(http.StatusConflict, "message_text_unavailable", "could not read the full original text of this message from the session log")
		}
		if len(target.text) > maxSendText {
			return claudeRewindPlan{}, rwErr(http.StatusConflict, "message_too_long", "message exceeds the 100 KiB composer limit and cannot be restored completely")
		}
	}
	if target.first == "" {
		return claudeRewindPlan{}, rwErr(http.StatusConflict, "not_ready", "cannot match an empty message in the rewind list")
	}
	steps := len(users) - targetIdx
	if steps < 1 || steps > rewindMaxSteps {
		return claudeRewindPlan{}, rwErr(http.StatusConflict, "not_ready", "message is too far back to rewind automatically")
	}
	ambiguous := false
	for i, u := range users {
		if i != targetIdx && u.first == target.first {
			ambiguous = true
			break
		}
	}
	var keptBuf strings.Builder
	for _, e := range entries[:target.idx] {
		if e.Gone {
			continue
		}
		for _, p := range e.Parts {
			if p.Kind == convo.KindText && p.Text != "" {
				keptBuf.WriteString(oneSpaced(p.Text) + "\n")
			}
		}
	}
	kept, seen := keptBuf.String(), map[string]bool{}
	var snippets []string
	for _, e := range entries[target.idx:] {
		if e.Gone || (e.Role != convo.RoleUser && e.Role != convo.RoleAssistant) {
			continue
		}
		for _, p := range e.Parts {
			if p.Kind != convo.KindText {
				continue
			}
			line := oneSpaced(firstNonEmptyLine(p.Text))
			if r := []rune(line); len(r) < 3 {
				continue
			} else if len(r) > 36 {
				line = string(r[:36])
			}
			if !seen[line] && !strings.Contains(kept, line) {
				seen[line] = true
				snippets = append(snippets, line)
			}
			if len(snippets) >= 12 {
				break
			}
		}
		if len(snippets) >= 12 {
			break
		}
	}
	return claudeRewindPlan{Steps: steps, TargetText: target.text, Ambiguous: ambiguous, WithdrawnSnippets: snippets}, nil
}

type claudeRewindCommit struct {
	started  bool
	remoteAt time.Time
}

type claudeRewindPane interface {
	verify(ctx context.Context) error
	press(ctx context.Context, keys ...string) error
	pickerEnter(ctx context.Context, targetText string) error
	commitEnter(ctx context.Context, targetText string, restoreCode bool, expectedFiles []string) (claudeRewindCommit, error)
	waitInFlight(ctx context.Context) error
	screen(ctx context.Context) (string, error)
	colorScreen(ctx context.Context) (string, error)
}

type claudeRewindResult struct {
	OK           bool      `json:"ok"`
	Text         string    `json:"text,omitempty"`
	Warning      string    `json:"warning,omitempty"`
	RestoredCode bool      `json:"restored_code,omitempty"`
	remoteAt     time.Time // remote host clock captured at commit time
	needsCleanup bool      // true when Phase-1 cleanup could not finish before returning
}

type rewindCloseStatus int

const (
	rewindCloseDone rewindCloseStatus = iota
	rewindCloseStillOpen
	rewindCloseIncomplete
)

// closeRewindPanel checks the screen and sends Esc only while a rewind picker
// or confirm panel is visibly open. Returns rewindCloseDone once verified
// closed (or after an identity change, when no further keys may be sent),
// rewindCloseStillOpen if the panel remains visibly open after two Esc presses,
// or rewindCloseIncomplete if screen reads/presses failed due to transport/ctx.
func closeRewindPanel(ctx context.Context, pane claudeRewindPane, recheck bool) rewindCloseStatus {
	checked := false
	pressed := false
	for range 2 {
		scr, serr := pane.screen(ctx)
		if serr != nil {
			if isIdentityErr(serr) {
				return rewindCloseDone
			}
			return rewindCloseIncomplete
		}
		if !isRewindPanelOpen(scr) {
			if recheck && !checked && !pressed {
				checked = true
				sleepCtx(ctx, claudeRewindStep)
				scr, serr = pane.screen(ctx)
				if serr != nil {
					if isIdentityErr(serr) {
						return rewindCloseDone
					}
					return rewindCloseIncomplete
				}
			}
			if !isRewindPanelOpen(scr) {
				return rewindCloseDone
			}
		}
		if perr := pane.press(ctx, "esc"); perr != nil {
			if isIdentityErr(perr) {
				return rewindCloseDone
			}
			return rewindCloseIncomplete
		}
		pressed = true
		sleepCtx(ctx, claudeRewindStep)
	}
	scr, serr := pane.screen(ctx)
	if serr != nil {
		if isIdentityErr(serr) {
			return rewindCloseDone
		}
		return rewindCloseIncomplete
	}
	if !isRewindPanelOpen(scr) {
		return rewindCloseDone
	}
	return rewindCloseStillOpen
}

func claudeRewind(ctx context.Context, pane claudeRewindPane, plan claudeRewindPlan) (claudeRewindResult, *rewindError) {
	if plan.Ambiguous {
		return claudeRewindResult{}, rwErr(http.StatusConflict, "ambiguous_message", "multiple messages share the same first line")
	}
	if plan.Steps < 1 || strings.TrimSpace(plan.TargetText) == "" {
		return claudeRewindResult{}, rwErr(http.StatusConflict, "not_ready", "invalid rewind target")
	}
	if plan.RestoreCode && len(plan.ExpectedFiles) == 0 {
		return claudeRewindResult{}, rwErr(http.StatusBadRequest, "bad_request", "files list is required when restore_code is true")
	}
	if err := pane.verify(ctx); err != nil {
		return claudeRewindResult{}, asRewindErr(err)
	}
	checkGreyed := func(cctx context.Context) func() string {
		return func() string {
			raw, err := pane.colorScreen(cctx)
			if err != nil || raw == "" || claudeOwner(sgr.ReplaceAllString(raw, ""), false) != "main" {
				return ""
			}
			return greyedIn(raw)
		}
	}

	initialScreen, err := pane.screen(ctx)
	if err != nil {
		return claudeRewindResult{}, asRewindErr(err)
	}
	if isRewindPanelOpen(initialScreen) {
		return claudeRewindResult{}, rwErr(http.StatusConflict, "not_ready", "a rewind panel is already open")
	}
	if ierr := idleInput(initialScreen, checkGreyed(ctx)); ierr != nil {
		switch {
		case errors.Is(ierr, errBusyAgent):
			return claudeRewindResult{}, rwErr(http.StatusConflict, "agent_busy", "the agent is busy")
		case errors.Is(ierr, errDraft):
			return claudeRewindResult{}, rwErr(http.StatusConflict, "input_not_empty", "terminal input box has unsent text")
		default:
			return claudeRewindResult{}, rwErr(http.StatusConflict, "not_ready", "terminal is not ready for rewind")
		}
	}

	initialAbove, _, _ := screenAboveAndBox(initialScreen)
	initialAboveNorm := oneSpaced(initialAbove)
	var markers []string
	for _, sn := range plan.WithdrawnSnippets {
		if strings.Contains(initialAboveNorm, sn) {
			markers = append(markers, sn)
		}
	}

	// Phase 1 cleanup: independent bounded context; waits for all in-flight
	// remote commands to finish first, then presses Esc ONLY while a rewind
	// panel is visibly open and NEVER after identity change or commit.
	committed := false
	keysSent := false
	cleanup := func(cause error) (claudeRewindResult, *rewindError) {
		re := asRewindErr(cause)
		if committed || isIdentityErr(re) || !keysSent {
			return claudeRewindResult{}, re
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeRewindCleanupWait)
		defer cancel()
		if pane.waitInFlight(cctx) != nil {
			return claudeRewindResult{needsCleanup: true}, re
		}
		switch closeRewindPanel(cctx, pane, ctx.Err() != nil) {
		case rewindCloseDone:
			return claudeRewindResult{}, re
		case rewindCloseStillOpen:
			return claudeRewindResult{}, rwErr(http.StatusBadGateway, "panel_not_closed", "could not close the rewind panel; press Esc in the terminal")
		default:
			return claudeRewindResult{needsCleanup: true}, re
		}
	}

	// Open picker via rapid double-Esc, step up to target, verify selection.
	keysSent = true
	if err := pane.press(ctx, "esc", "esc"); err != nil {
		return cleanup(err)
	}
	var pk rewindPicker
	if !waitScreen(ctx, pane, claudeRewindWait, func(scr string) bool { var ok bool; pk, ok = parseRewindPicker(scr); return ok }) {
		if ctx.Err() != nil {
			return cleanup(ctx.Err())
		}
		return cleanup(rwErr(http.StatusConflict, "picker_not_opened", "rewind picker did not open"))
	}
	if pk.matchCount(plan.TargetText) > 1 {
		return cleanup(rwErr(http.StatusConflict, "ambiguous_message", "multiple messages share the same first line"))
	}
	for range plan.Steps {
		if err := pane.press(ctx, "up"); err != nil {
			return cleanup(err)
		}
	}
	var pickedOK, sawPicker bool
	var lastPk rewindPicker
	waitScreen(ctx, pane, claudeRewindWait, func(scr string) bool {
		p, ok := parseRewindPicker(scr)
		if !ok {
			return false
		}
		sawPicker, lastPk = true, p
		if p.matchCount(plan.TargetText) > 1 {
			return true
		}
		pickedOK = matchPickerSelection(p.selected, plan.TargetText)
		return pickedOK
	})
	if ctx.Err() != nil {
		return cleanup(ctx.Err())
	}
	if sawPicker && lastPk.matchCount(plan.TargetText) > 1 {
		return cleanup(rwErr(http.StatusConflict, "ambiguous_message", "multiple messages share the same first line"))
	}
	if !pickedOK {
		return cleanup(rwErr(http.StatusConflict, "target_mismatch", "selected message in rewind list does not match target"))
	}

	// Atomically verify picker + selected target on host before sending Enter
	// (so an intervening Enter in the terminal cannot turn this into a confirm-screen Enter).
	if err := pane.pickerEnter(ctx, plan.TargetText); err != nil {
		return cleanup(err)
	}
	var cf rewindConfirm
	if !waitScreen(ctx, pane, claudeRewindWait, func(scr string) bool { var ok bool; cf, ok = parseRewindConfirm(scr); return ok }) {
		if ctx.Err() != nil {
			return cleanup(ctx.Err())
		}
		return cleanup(rwErr(http.StatusConflict, "not_ready", "rewind confirmation screen did not appear"))
	}
	if !matchConfirmQuote(cf.quoted, plan.TargetText) {
		return cleanup(rwErr(http.StatusConflict, "confirm_mismatch", "quoted message on confirmation screen does not match target"))
	}
	wantOpt := rewindConfirmOpt
	if plan.RestoreCode {
		wantOpt = rewindConfirmCodeOpt
	}
	targetOptIdx, curOptIdx := -1, -1
	for i, o := range cf.options {
		if strings.EqualFold(o.label, wantOpt) {
			targetOptIdx = i
		}
		if o.selected {
			curOptIdx = i
		}
	}
	if targetOptIdx < 0 {
		if plan.RestoreCode {
			return cleanup(rwErr(http.StatusConflict, "no_code_changes", "no restorable code changes after this message"))
		}
		return cleanup(rwErr(http.StatusConflict, "option_not_found", "conversation-only restore option not found"))
	}
	if curOptIdx < 0 {
		return cleanup(rwErr(http.StatusConflict, "option_not_found", "selected restore option not found"))
	}
	for delta := targetOptIdx - curOptIdx; delta != 0; {
		key := "down"
		if delta < 0 {
			key, delta = "up", delta+1
		} else {
			delta--
		}
		if err := pane.press(ctx, key); err != nil {
			return cleanup(err)
		}
	}
	if targetOptIdx != curOptIdx && !waitScreen(ctx, pane, claudeRewindWait, func(scr string) bool {
		c, ok := parseRewindConfirm(scr)
		sel, selOK := c.selectedOption()
		return ok && selOK && strings.EqualFold(sel.label, wantOpt)
	}) {
		if ctx.Err() != nil {
			return cleanup(ctx.Err())
		}
		return cleanup(rwErr(http.StatusConflict, "confirm_mismatch", "could not select expected restore option"))
	}

	// Mandatory final screen check immediately before commit Enter: exact option,
	// quoted original text, and (when restoring code) exact restored file set must ALL match.
	preCommitScr, err := pane.screen(ctx)
	if err != nil {
		return cleanup(err)
	}
	preCf, ok := parseRewindConfirm(preCommitScr)
	selOpt, selOK := preCf.selectedOption()
	if !ok || !selOK || !strings.EqualFold(selOpt.label, wantOpt) || !matchConfirmQuote(preCf.quoted, plan.TargetText) {
		return cleanup(rwErr(http.StatusConflict, "confirm_mismatch", "confirmation screen did not match expected option and target"))
	}
	if plan.RestoreCode && !matchRewindCodeFiles(preCf.codeDesc, preCf.codeDescOK, plan.ExpectedFiles) {
		return cleanup(rwErr(http.StatusConflict, "code_mismatch", "files to restore do not match the expected list"))
	}

	// Phase 2: COMMIT — atomic remote verify+Enter. Only once the remote commit
	// script has actually started (and did not actively exit before Enter) do we
	// enter the committed state where Esc is forbidden.
	commit, enterErr := pane.commitEnter(ctx, plan.TargetText, plan.RestoreCode, plan.ExpectedFiles)
	if enterErr != nil {
		code := asRewindErr(enterErr).code
		if !commit.started || isIdentityErr(enterErr) || code == "confirm_mismatch" || code == "code_mismatch" {
			return cleanup(enterErr)
		}
	}
	committed = true
	vctx, vcancel := context.WithTimeout(context.WithoutCancel(ctx), claudeRewindPostWait+2*time.Second)
	defer vcancel()
	var postScr, postBox string
	confirmed := waitScreen(vctx, pane, claudeRewindPostWait, func(scr string) bool {
		if strings.Contains(scr, "esc to interrupt") {
			return false
		}
		if _, ok := parseRewindPicker(scr); ok {
			return false
		}
		if _, ok := parseRewindConfirm(scr); ok {
			return false
		}
		above, box, hasBox := screenAboveAndBox(scr)
		if !hasBox {
			return false
		}
		if len(markers) > 0 {
			aboveNorm := oneSpaced(above)
			for _, m := range markers {
				if strings.Contains(aboveNorm, m) {
					return false
				}
			}
			postScr, postBox = scr, box
			return true
		}
		if matchInputRefill(box, plan.TargetText) {
			postScr, postBox = scr, box
			return true
		}
		return false
	})
	if !confirmed {
		return claudeRewindResult{}, rwErr(http.StatusBadGateway, "rewind_uncertain", "could not confirm whether rewind succeeded; check the terminal")
	}

	res := claudeRewindResult{OK: true, Text: plan.TargetText, RestoredCode: plan.RestoreCode, remoteAt: commit.remoteAt}
	if idleInput(postScr, checkGreyed(vctx)) == nil {
		return res, nil
	}
	if !matchInputRefill(postBox, plan.TargetText) || pane.press(vctx, "ctrl+c") != nil ||
		!waitScreen(vctx, pane, claudeRewindClearWait, func(scr string) bool { return idleInput(scr, checkGreyed(vctx)) == nil }) {
		res.Warning = "input_not_cleared"
	}
	return res, nil
}

func waitScreen(ctx context.Context, pane claudeRewindPane, within time.Duration, ok func(string) bool) bool {
	until := time.Now().Add(within)
	for {
		if scr, err := pane.screen(ctx); err == nil && ok(scr) {
			return true
		}
		if time.Now().After(until) || ctx.Err() != nil {
			return false
		}
		sleepCtx(ctx, claudeRewindStep)
	}
}
