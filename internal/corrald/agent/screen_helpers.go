//go:build linux

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Screen-parsing and digest helpers ported verbatim from convoGated's awk/sed
// pipelines (convo_view.go, convo_panel.go, convo_card.go,
// convo_opencode_input.go, convo_agy_input.go).

const claudeOwnerFoot = 12

var (
	// Claude owner / box / palette patterns (convo_view.go).
	ownerRuleRe        = regexp.MustCompile(`^[ \t]*(?:────────|━━━━━━━━)`)
	ownerPromptRe      = regexp.MustCompile(`^[ \t]*❯`)
	ownerOtherMainRe   = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?◯ main(?:[ \t]|$)`)
	ownerStopMainRe    = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?◯ main[ \t]*$`)
	ownerListCursorRe  = regexp.MustCompile(`^[ \t]*❯[ \t]+(?:◯|●) `)
	ownerAgentRowRe    = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?(?:◯|●) [^ \t]+(?:[ \t][ \t]+.*|[ \t]*)$`)
	ownerMainRowRe     = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?(?:◯|●) main[ \t]*$`)
	ownerChoiceRe      = regexp.MustCompile(`^[ \t]*[0-9]+\.[ \t]`)
	ownerPlaceholderRe = regexp.MustCompile(`^Message @[A-Za-z0-9_.:-]+…?…$`)

	paletteLeadingPromptRe = regexp.MustCompile(`^[ \t]*(?:❯[ \t]*)?`)
	paletteCmdRe           = regexp.MustCompile(`^/[A-Za-z0-9_-]+(?:[ \t]|$)`)
	paletteStopRe          = regexp.MustCompile(`^(?:❯|[0-9]+\.)`)

	// Steady / panel / look digest patterns (convo_panel.go).
	panelSGRRe     = regexp.MustCompile(`\x1b\[[0-9;:]*m`)
	blinkingMarkRe = regexp.MustCompile(`(?m)^([ \t]*)(?:●|⏺)`)
	spinnerLineRe  = regexp.MustCompile(`(?m)^(?:\*|·|✢|✳|✶|✻|✽) (?:[A-Za-z0-9 -]*….*|Waiting for [0-9]+ background (?:task|agent|shell)s?.*)$`)
	agentRowRe     = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?(?:●|◯)[ \t]`)
	agentMainRe    = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?(?:●|◯)[ \t]+main[ \t]*$`)
	rowAgeRe       = regexp.MustCompile(`[ \t]+[0-9]+[hms][ \t]*$`)
	blankLineRe    = regexp.MustCompile(`^[ \t]*$`)

	askPureRuleRe  = regexp.MustCompile(`^[ \t]*[─━╌▔]{8,}[ \t]*$`)
	askHintRuleRe  = regexp.MustCompile(`^[ \t]*[─━╌▔]{2,}.*[─━╌▔]{2,}[ \t]*$`)
	overlinePureRe = regexp.MustCompile(`^ ?▔{8,}[ \t]*$`)
	overlineBarRe  = regexp.MustCompile(`^ ?▔{2,}.*▔+[ \t]*$`)
	overlineTailRe = regexp.MustCompile(`^ ?[^ \t].*[ \t]▔+[ \t]*$`)
	solidRuleRe    = regexp.MustCompile(`^[ \t]*[─━]{8,}[ \t]*$`)
	promptTopRe    = regexp.MustCompile(`^[ \t]*❯(?:[ \t]|$)`)
	dialogTitleRe  = regexp.MustCompile(`^[ \t]*[A-Z][A-Za-z0-9 ,-]{2,}\?[ \t]*$`)
	askMultiTabRe  = regexp.MustCompile(`^[ \t]*(?:←[ \t]+)?(?:☐|☒|☑)[ \t]+[^ \t].*[ \t]+(?:✔|✓)[ \t]+Submit(?:[ \t]+→)?[ \t]*$`)
	askLoneTabRe   = regexp.MustCompile(`^[ \t]*(?:☐|☒)[ \t]+[^ \t]`)
	askOptLineRe   = regexp.MustCompile(`^[ \t]*(❯)?[ \t]*([0-9]{1,2})\.[ \t]+(.*)$`)
	askSubmitLnRe  = regexp.MustCompile(`^[ \t]*(?:❯[ \t]+)?Submit[ \t]*$`)
	askChatLineRe  = regexp.MustCompile(`^[ \t]*(?:[0-9]{1,2}\.[ \t]+)?Chat about this[ \t]*$`)
	askReviewHdRe  = regexp.MustCompile(`^[ \t]*Review your answers[ \t]*$`)
	askHintSegRe   = regexp.MustCompile(`^(?:Enter to (?:select|confirm|submit)|(?:[↑↓←→/]+|Tab/Arrow keys|Arrow keys) to navigate|Tab to (?:switch questions|amend)|n to add notes|Esc to cancel|shift\+tab to approve with this feedback|ctrl\+g to edit in Vim(?: .*)?)$`)
	askNavSegRe    = regexp.MustCompile(`Enter to (?:select|confirm|submit)|to navigate|Tab to switch questions`)
	bgFooterRe     = regexp.MustCompile(`[0-9]+ background (?:task|agent|shell)s?|[0-9]+ local agents?|↓ to manage|ctrl\+o to expand|ctrl\+t to hide`)
	bgStatusRe     = regexp.MustCompile(`│.*(?:Ctx |\$[0-9])|(?:Ctx |\$[0-9]).*│`)
	bgModeRe       = regexp.MustCompile(`^[ \t]*(?:⏵|⏸|\? for shortcuts|Tip:[ \t])`)

	ocSpinnerRe = regexp.MustCompile(`(?m)^([ \t]*┃[ \t]+)[\x{2800}-\x{28FF}]`)
	ocBarRe     = regexp.MustCompile(`(?m)^([ \t]*)[⬝■]+ `)

	// Card region patterns (convo_card.go).
	cardSGRRe    = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	cardRuleRe   = regexp.MustCompile(`^[ \t\f]*[─━]{8}`)
	cardChoiceRe = regexp.MustCompile(`^[ \t\f]*[0-9]+\.[ \t\f]`)

	// Image placeholder check for taken() with haspic.
	claudeImageTagRe = regexp.MustCompile(`(?s)\[Image #[0-9].*\]`)
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// tailNonBlankLines mirrors convoGated's screen() awk:
//
//	printf '%s\n' "$s" | awk -v n="$rd" '{ a[NR] = $0 } NF { last = NR } END { for (i = last - n + 1; i <= last; i++) if (i > 0) print a[i] }'
//
// where $s has already had trailing newlines stripped by shell command substitution.
func tailNonBlankLines(raw string, rd int) []byte {
	if rd <= 0 {
		return nil
	}
	s := strings.TrimRight(raw, "\n")
	lines := strings.Split(s, "\n")
	last := 0 // 1-indexed matching awk NR
	for i, l := range lines {
		if hasAwkFields(l) {
			last = i + 1
		}
	}
	if last == 0 {
		return nil
	}
	start := last - rd + 1
	if start < 1 {
		start = 1
	}
	var b bytes.Buffer
	for i := start; i <= last; i++ {
		b.WriteString(lines[i-1])
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// hasAwkFields reports whether s has at least one whitespace-separated field
// under POSIX awk's default FS=" " (spaces and tabs).
func hasAwkFields(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return true
		}
	}
	return false
}

func ownerSqueeze(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\u00a0' {
			return -1
		}
		return r
	}, s)
}

func ownerAgents(foot []string) []bool {
	mask := make([]bool, len(foot))
	for k := 0; k < len(foot); {
		if !ownerAgentRowRe.MatchString(foot[k]) {
			k++
			continue
		}
		e, hasMain := k, false
		for e < len(foot) && ownerAgentRowRe.MatchString(foot[e]) {
			hasMain = hasMain || ownerMainRowRe.MatchString(foot[e])
			e++
		}
		for x := k; x < e && hasMain; x++ {
			mask[x] = true
		}
		k = e
	}
	return mask
}

func ownerModeLine(l string) bool {
	t := strings.TrimLeft(l, " \t\u00a0")
	return strings.HasPrefix(t, "⏵") || strings.HasPrefix(t, "⏸") || strings.HasPrefix(ownerSqueeze(l), "?forshortcuts")
}

func ownerBoxStatus(foot []string, agents []bool) bool {
	last := ""
	for k, l := range foot {
		if agents[k] {
			return ownerModeLine(last)
		}
		if ownerSqueeze(l) != "" {
			last = l
		}
	}
	return false
}

func ownerPlaceholder(box []string) bool {
	parts := make([]string, 0, len(box))
	for k, l := range box {
		if k == 0 {
			l = ownerPromptRe.ReplaceAllString(l, "")
		}
		if l = strings.Trim(l, " \t\u00a0"); l != "" {
			parts = append(parts, l)
		}
	}
	return ownerPlaceholderRe.MatchString(strings.Join(parts, " "))
}

// claudeOwner mirrors claudeOwnerAwk on `printf '%s\n' "$os"`.
func claudeOwner(osStr string, after bool) string {
	lines := strings.Split(osStr, "\n")
	for k, l := range lines {
		lines[k] = strings.TrimSuffix(l, "\r")
	}
	n := len(lines)
	for n > 0 && ownerSqueeze(lines[n-1]) == "" {
		n--
	}
	for i := n - 1; i >= 1; i-- {
		if !ownerPromptRe.MatchString(lines[i]) || !ownerRuleRe.MatchString(lines[i-1]) {
			continue
		}
		end := -1
		for j := i + 1; j < n; j++ {
			if ownerRuleRe.MatchString(lines[j]) {
				end = j
				break
			}
		}
		if end < 0 {
			continue
		}
		foot := lines[end+1 : n]
		for _, l := range foot {
			if ownerOtherMainRe.MatchString(l) {
				return "subagent"
			}
		}
		agents := ownerAgents(foot)
		var saidB strings.Builder
		for _, l := range foot {
			saidB.WriteString(ownerSqueeze(l))
		}
		said := saidB.String()
		hover := ownerBoxStatus(foot, agents)
		for k, l := range foot {
			if ownerListCursorRe.MatchString(l) && !(hover && agents[k]) {
				return "list"
			}
		}
		if strings.Contains(said, "↑/↓toselect") {
			return "list"
		}
		if strings.Contains(said, "entertoreturn") {
			return "sessions"
		}
		other := 0
		for k, l := range foot {
			if (strings.Contains(l, "❯") && !(hover && agents[k])) || ownerChoiceRe.MatchString(l) {
				return "none"
			}
			if !agents[k] {
				other++
			}
		}
		if strings.Contains(said, "Esctocancel") || strings.Contains(said, "Entertoselect") || strings.Contains(said, "Entertoconfirm") {
			return "none"
		}
		if other > claudeOwnerFoot {
			return "none"
		}
		shown := false
		for k, l := range foot {
			if agents[k] && ownerMainRowRe.MatchString(l) && strings.Contains(l, "●") {
				shown = true
			}
		}
		if !after && !shown {
			if ownerPlaceholder(lines[i:end]) {
				return "subagent"
			}
			var boxB strings.Builder
			for j := i; j < end; j++ {
				t := lines[j]
				if j == i {
					t = ownerPromptRe.ReplaceAllString(t, "")
				}
				boxB.WriteString(ownerSqueeze(t))
			}
			if boxB.String() == "describeataskforanewsession" {
				return "sessions"
			}
		}
		return "main"
	}
	return "none"
}

// claudeHasOtherMainLine mirrors the stop-gate awk check:
// `/^[ \t]*(❯[ \t]+)?◯ main[ \t]*$/ { f = 1 } END { exit !f }`.
func claudeHasOtherMainLine(osStr string) bool {
	for _, l := range strings.Split(osStr, "\n") {
		if ownerStopMainRe.MatchString(l) {
			return true
		}
	}
	return false
}

// claudePalette mirrors claudePaletteAwk on `printf '%s\n' "$os"`.
func claudePalette(osStr string) bool {
	count := 0
	indent := 0
	for _, rawLine := range strings.Split(osStr, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if blankLineRe.MatchString(line) {
			continue
		}
		if isPaletteRuleOnly(line) {
			continue
		}
		text := paletteLeadingPromptRe.ReplaceAllString(line, "")
		if paletteCmdRe.MatchString(text) {
			count++
			indent = firstNonSpaceByte(line)
			continue
		}
		if count > 0 && firstNonSpaceByte(line) > indent && !paletteStopRe.MatchString(text) {
			continue
		}
		count = 0
	}
	return count > 0
}

func isPaletteRuleOnly(s string) bool {
	for _, r := range s {
		if r != '─' && r != '━' && r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

func firstNonSpaceByte(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return i
		}
	}
	return -1
}

// claudeBoxText mirrors claudeBoxAwk on `printf '%s\n' "$bs"` (where bs already
// had trailing newlines stripped by shell command substitution).
// Returns (textWithoutTrailingNewline, found).
func claudeBoxText(bs string) (string, bool) {
	lines := strings.Split(bs, "\n")
	for k, l := range lines {
		lines[k] = strings.TrimSuffix(l, "\r")
	}
	for i := len(lines) - 1; i >= 1; i-- {
		if !ownerPromptRe.MatchString(lines[i]) || !ownerRuleRe.MatchString(lines[i-1]) {
			continue
		}
		end := -1
		for j := i + 1; j < len(lines); j++ {
			if ownerRuleRe.MatchString(lines[j]) {
				end = j
				break
			}
		}
		if end < 0 {
			continue
		}
		var b strings.Builder
		for j := i; j < end; j++ {
			t := lines[j]
			if j == i {
				t = ownerPromptRe.ReplaceAllString(t, "")
			}
			if j > i {
				b.WriteByte('\n')
			}
			b.WriteString(strings.Join(strings.FieldsFunc(t, func(r rune) bool {
				return r == ' ' || r == '\t' || r == '\u00a0'
			}), " "))
		}
		return b.String(), true
	}
	return "", false
}

// claudeBoxDigest mirrors `printf '%s\n' "$bs" | awk 'BOXAWK' | sha256sum`.
func claudeBoxDigest(bs string) string {
	txt, ok := claudeBoxText(bs)
	if !ok {
		return sha256Hex(nil)
	}
	return sha256Hex([]byte(txt + "\n"))
}

// claudeBoxSqueeze mirrors claudeBoxSqueezeAwk (with command substitution
// stripping trailing newline).
func claudeBoxSqueeze(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\u00a0' {
			return -1
		}
		return r
	}, s)
}

func isAskRule(s string) bool {
	return askPureRuleRe.MatchString(s) || (askHintRuleRe.MatchString(s) && !strings.Contains(s, "❯"))
}

func isOverlineLine(s string) bool {
	if strings.Contains(s, "❯") || isIndented(s) || askOptLineRe.MatchString(s) {
		return false
	}
	return overlinePureRe.MatchString(s) || overlineBarRe.MatchString(s) || overlineTailRe.MatchString(s)
}

func isOverlineChrome(plain []string, i int) bool {
	if i < 0 || i >= len(plain) || !isOverlineLine(plain[i]) {
		return false
	}
	blanks := 0
	for j := i + 1; j < len(plain); j++ {
		if blankLineRe.MatchString(plain[j]) {
			blanks++
			if blanks > 2 {
				return false
			}
			continue
		}
		nxt := plain[j]
		return solidRuleRe.MatchString(nxt) ||
			promptTopRe.MatchString(nxt) ||
			askMultiTabRe.MatchString(nxt) ||
			askLoneTabRe.MatchString(nxt) ||
			askReviewHdRe.MatchString(nxt) ||
			dialogTitleRe.MatchString(nxt)
	}
	return false
}

func isAskHintLine(s string) bool {
	t := strings.Trim(s, " \t")
	if t == "" || strings.Contains(t, "❯") {
		return false
	}
	if t == "ctrl+g to edit in Vim" || strings.HasPrefix(t, "ctrl+g to edit in Vim · ") {
		return true
	}
	parts := strings.Split(t, "·")
	n := 0
	for _, p := range parts {
		p = strings.Trim(p, " \t")
		if p == "" {
			continue
		}
		if !askHintSegRe.MatchString(p) {
			return false
		}
		n++
	}
	return n > 0
}

func isBgFooterLine(s string) bool {
	if strings.Contains(s, "❯") || askOptLineRe.MatchString(s) {
		return false
	}
	return bgModeRe.MatchString(s) || bgFooterRe.MatchString(s) || bgStatusRe.MatchString(s)
}

func isIndented(s string) bool {
	return strings.HasPrefix(s, "  ") || strings.HasPrefix(s, "\t")
}

func askDialogBounds(lines []string) (top, end int, ok bool) {
	n := len(lines)
	for n > 0 && blankLineRe.MatchString(lines[n-1]) {
		n--
	}
	if n < 2 {
		return 0, 0, false
	}
	k := n - 1
	sawAskHint, sawChat := false, false
	for k >= 0 {
		l := lines[k]
		if blankLineRe.MatchString(l) {
			k--
			continue
		}
		if agentRowRe.MatchString(l) {
			j, hasMain := k, false
			for j >= 0 && agentRowRe.MatchString(lines[j]) {
				hasMain = hasMain || agentMainRe.MatchString(lines[j])
				j--
			}
			if hasMain {
				k = j
				continue
			}
			break
		}
		if isAskHintLine(l) {
			if askNavSegRe.MatchString(l) {
				sawAskHint = true
			}
			k--
			continue
		}
		if k >= 1 && !strings.Contains(l, "❯") && !askOptLineRe.MatchString(l) && !blankLineRe.MatchString(lines[k-1]) && !strings.Contains(lines[k-1], "❯") && !askOptLineRe.MatchString(lines[k-1]) {
			prev := strings.Trim(lines[k-1], " \t")
			cur := strings.Trim(l, " \t")
			if joined := prev + " " + cur; isAskHintLine(joined) {
				if askNavSegRe.MatchString(joined) {
					sawAskHint = true
				}
				k -= 2
				continue
			}
			if joined := prev + cur; isAskHintLine(joined) {
				if askNavSegRe.MatchString(joined) {
					sawAskHint = true
				}
				k -= 2
				continue
			}
		}
		if isAskRule(l) {
			k--
			continue
		}
		if askChatLineRe.MatchString(l) {
			sawChat = true
			k--
			continue
		}
		if spinnerLineRe.MatchString(l) || isBgFooterLine(l) {
			k--
			continue
		}
		break
	}
	end = k
	if end < 1 {
		return 0, 0, false
	}
	submitCursor := 0
	scanStart := end
	if askSubmitLnRe.MatchString(lines[end]) {
		if strings.Contains(lines[end], "❯") {
			submitCursor = 1
		}
		scanStart = end - 1
	}
	optCount, lastNum, cursorCount, firstOpt, maxOptLine := 0, 0, 0, -1, -1
	opt1Text, opt2Text := "", ""
	badOpt := false
	limit := scanStart - 80
	if limit < 0 {
		limit = 0
	}
	for i := scanStart; i >= limit; i-- {
		l := lines[i]
		if isAskRule(l) || askMultiTabRe.MatchString(l) || askLoneTabRe.MatchString(l) || askReviewHdRe.MatchString(l) {
			break
		}
		if m := askOptLineRe.FindStringSubmatch(l); m != nil {
			num := 0
			for _, ch := range m[2] {
				num = num*10 + int(ch-'0')
			}
			if optCount == 0 {
				lastNum = num
				maxOptLine = i
			} else if num != lastNum-1 {
				badOpt = true
				break
			} else {
				lastNum = num
			}
			optCount++
			if m[1] != "" {
				cursorCount++
			}
			if num == 1 {
				opt1Text = strings.Trim(m[3], " \t")
			} else if num == 2 {
				opt2Text = strings.Trim(m[3], " \t")
			}
			firstOpt = i
			if num == 1 {
				break
			}
		} else if strings.Contains(l, "❯") {
			badOpt = true
			break
		}
	}
	if !badOpt && maxOptLine >= 0 {
		seenBlank := false
		for m := maxOptLine + 1; m <= end; m++ {
			if askSubmitLnRe.MatchString(lines[m]) {
				continue
			}
			if blankLineRe.MatchString(lines[m]) {
				seenBlank = true
				continue
			}
			if seenBlank && !isIndented(lines[m]) {
				badOpt = true
				break
			}
		}
	}
	if badOpt || optCount < 2 || lastNum != 1 || cursorCount+submitCursor != 1 || firstOpt <= 0 {
		return 0, 0, false
	}
	if optCount == 2 && opt1Text == "Submit answers" && opt2Text == "Cancel" {
		revLimit := firstOpt - 60
		if revLimit < 0 {
			revLimit = 0
		}
		revIdx := -1
		for j := firstOpt - 1; j >= revLimit; j-- {
			l := lines[j]
			if isAskRule(l) || askMultiTabRe.MatchString(l) {
				break
			}
			if askReviewHdRe.MatchString(l) {
				revIdx = j
			}
		}
		if revIdx >= 0 {
			top = revIdx
			for u := revIdx - 1; u >= 0; u-- {
				if blankLineRe.MatchString(lines[u]) {
					continue
				}
				if askMultiTabRe.MatchString(lines[u]) {
					top = u
				}
				break
			}
			return top, end, true
		}
	}
	if !sawAskHint && !sawChat {
		return 0, 0, false
	}
	tabLimit := firstOpt - 60
	if tabLimit < 0 {
		tabLimit = 0
	}
	firstNonBlank, nonBlankCount := -1, 0
	hitRule := false
	for j := firstOpt - 1; j >= tabLimit; j-- {
		l := lines[j]
		if isAskRule(l) || isOverlineChrome(lines, j) {
			hitRule = true
			break
		}
		if !blankLineRe.MatchString(l) {
			firstNonBlank = j
			nonBlankCount++
		}
	}
	if (hitRule || tabLimit == 0) && firstNonBlank >= 0 && nonBlankCount >= 2 &&
		(askMultiTabRe.MatchString(lines[firstNonBlank]) || askLoneTabRe.MatchString(lines[firstNonBlank])) {
		return firstNonBlank, end, true
	}
	return 0, 0, false
}

func agentRowsAged(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	n := len(lines)
	for n > 0 && blankLineRe.Match(lines[n-1]) {
		n--
	}
	i, hasMain := n, false
	for i > 0 && agentRowRe.Match(lines[i-1]) {
		i--
		hasMain = hasMain || agentMainRe.Match(lines[i])
	}
	if hasMain {
		for k := i; k < n; k++ {
			for rowAgeRe.Match(lines[k]) {
				lines[k] = rowAgeRe.ReplaceAll(lines[k], nil)
			}
		}
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

// steadyScreen mirrors steadyPipe (`tr -d '\000' | LC_ALL=C awk 'steadyAwk'`).
func steadyScreen(raw []byte) []byte {
	b := bytes.ReplaceAll(raw, []byte{0}, nil)
	if len(b) == 0 {
		return b
	}
	trimmed := bytes.TrimSuffix(b, []byte("\n"))
	rawLines := bytes.Split(trimmed, []byte("\n"))
	plain := make([]string, len(rawLines))
	for i, rl := range rawLines {
		plain[i] = strings.TrimSuffix(string(rl), "\r")
	}
	if top, end, ok := askDialogBounds(plain); ok {
		var out bytes.Buffer
		for i := top; i <= end; i++ {
			out.Write(rawLines[i])
			out.WriteByte('\n')
		}
		return out.Bytes()
	}
	normLines := make([][]byte, len(rawLines))
	for i, rl := range rawLines {
		if isOverlineChrome(plain, i) {
			normLines[i] = []byte("▔")
		} else {
			normLines[i] = rl
		}
	}
	out := agentRowsAged(append(bytes.Join(normLines, []byte("\n")), '\n'))
	out = spinnerLineRe.ReplaceAll(out, nil)
	out = blinkingMarkRe.ReplaceAll(out, []byte("${1} "))
	return out
}

func steadyDigest(raw []byte) string {
	return sha256Hex(steadyScreen(raw))
}

// panelDigest mirrors panelDigestPipe (`tr -d '\000' | sed SGR | awk steadyAwk | sha256sum`).
func panelDigest(raw []byte) string {
	stripped := panelSGRRe.ReplaceAll(bytes.ReplaceAll(raw, []byte{0}, nil), nil)
	return sha256Hex(steadyScreen(stripped))
}

// lookDigest mirrors lookDigestPipe.
func lookDigest(raw []byte) string {
	b := panelSGRRe.ReplaceAll(bytes.ReplaceAll(raw, []byte{0}, nil), nil)
	b = ocBarRe.ReplaceAll(ocSpinnerRe.ReplaceAll(b, []byte("${1} ")), []byte("${1}■ "))
	b = blinkingMarkRe.ReplaceAll(spinnerLineRe.ReplaceAll(agentRowsAged(b), nil), []byte("${1} "))
	return sha256Hex(b)
}

func cardRightNote(q string) bool {
	t := strings.TrimLeft(q, " ")
	return len(q)-len(t) >= 24 && t != ""
}

// cardRegionDigest mirrors `tr -d '\000' | awk 'CARDAWK' | sha256sum`.
func cardRegionDigest(raw []byte) string {
	b := bytes.ReplaceAll(raw, []byte{0}, nil)
	if len(b) == 0 {
		return sha256Hex(nil)
	}
	s := string(b)
	if strings.Contains(s, "\x01corral: unread") {
		return sha256Hex(nil)
	}
	s = strings.ReplaceAll(s, "\r", "")
	trimmed := strings.TrimSuffix(s, "\n")
	rawLines := strings.Split(trimmed, "\n")
	plain := make([]string, len(rawLines))
	for i, l := range rawLines {
		plain[i] = cardSGRRe.ReplaceAllString(l, "")
	}
	n := len(rawLines)
	for n >= 1 && rawLines[n-1] == "" {
		n--
	}
	mark := -1
	for i := n - 1; i >= 1; i-- {
		q := strings.TrimLeft(plain[i], " \t\f")
		if strings.HasPrefix(q, "❯") && cardRuleRe.MatchString(plain[i-1]) {
			mark = i
			break
		}
	}
	if mark < 0 {
		return sha256Hex(nil)
	}
	end := -1
	for j := mark + 1; j < n; j++ {
		if cardRuleRe.MatchString(plain[j]) {
			end = j
			break
		}
	}
	if end < 0 {
		return sha256Hex(nil)
	}
	foot := plain[end+1 : n]
	agents := ownerAgents(foot)
	hover := ownerBoxStatus(foot, agents)
	other := 0
	for j, l := range foot {
		if agents[j] {
			if strings.Contains(l, "❯") && !hover {
				return sha256Hex(nil)
			}
			continue
		}
		if strings.Contains(l, "❯") || cardChoiceRe.MatchString(l) {
			return sha256Hex(nil)
		}
		other++
	}
	if other > 10 {
		return sha256Hex(nil)
	}
	top := mark - 1
	star := -1
	for k := top - 1; k >= 0 && k >= top-60; k-- {
		if strings.HasPrefix(plain[k], "✦ ") {
			star = k
			break
		}
		q := plain[k]
		if strings.Trim(q, " \t\f") != "" && !strings.HasPrefix(q, "  ") && !cardRightNote(q) {
			return sha256Hex(nil)
		}
	}
	if star < 0 {
		return sha256Hex(nil)
	}
	var out bytes.Buffer
	for k := star; k <= end; k++ {
		out.WriteString(rawLines[k])
		out.WriteByte('\n')
	}
	return sha256Hex(out.Bytes())
}

// ocBox mirrors ocBoxPipe on `printf '%s\n' "$ocs"` (where ocs already had
// trailing newlines stripped by shell command substitution).
func ocBox(ocs string) ([]string, bool) {
	s := strings.NewReplacer("\x00", "", "\r", "").Replace(ocs)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	e, seen := -1, 0
	for i := len(lines) - 1; i >= 0 && seen < 3; i-- {
		t := strings.TrimLeft(lines[i], " \t")
		if t == "" {
			continue
		}
		seen++
		if strings.HasPrefix(t, "╹▀") {
			e = i
			break
		}
	}
	if e < 0 {
		return nil, false
	}
	top := e
	for top > 0 && strings.HasPrefix(strings.TrimLeft(lines[top-1], " \t"), "┃") {
		top--
	}
	if top == e {
		return nil, false
	}
	return lines[top : e+1], true
}

func ocBoxDigest(ocs string) (string, bool) {
	box, ok := ocBox(ocs)
	if !ok {
		return "", false
	}
	return sha256Hex([]byte(strings.Join(box, "\n") + "\n")), true
}

func isAgyRule(s string) bool {
	t := strings.Trim(s, " \t")
	if len(t) < 36 { // 12 × "─" (3 UTF-8 bytes each)
		return false
	}
	return strings.ReplaceAll(t, "─", "") == ""
}

// agyBox mirrors agyBoxPipe on `printf '%s\n' "$ags"`.
func agyBox(ags string) ([]string, bool) {
	s := strings.NewReplacer("\x00", "", "\r", "").Replace(ags)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	var rules []int
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < 24; i-- {
		t := strings.TrimLeft(lines[i], " \t")
		if t == "" {
			continue
		}
		seen++
		if isAgyRule(lines[i]) {
			rules = append(rules, i)
			if len(rules) >= 6 {
				break
			}
		}
	}
	for k := 0; k < len(rules); k++ {
		bot := rules[k]
		top := -1
		if k+1 < len(rules) {
			top = rules[k+1]
		} else {
			for i := bot - 2; i >= 0 && bot-i <= 40; i-- {
				if isAgyRule(lines[i]) {
					top = i
					break
				}
			}
		}
		if top < 0 || bot-top < 2 || bot-top > 40 {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(lines[top+1], " \t"), ">") {
			return lines[top : bot+1], true
		}
	}
	return nil, false
}

func agyBoxDigest(ags string) (string, bool) {
	box, ok := agyBox(ags)
	if !ok {
		return "", false
	}
	return sha256Hex([]byte(strings.Join(box, "\n") + "\n")), true
}
