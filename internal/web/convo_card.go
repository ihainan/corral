package web

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Claude Code's own cards above its input box: its you-should-know plugin
// (a model of its own watching the session) offers something to know, as
// "✦ Heads up · …" or "✦ You should know · …" with keys to answer it
// ("1: Learn more   2: Knew this already   0: Dismiss"), opened out to an
// explanation on 1. They are drawn on the screen only: the log has none of
// them (see plans/headsup-card.md), so they are read off the screen, as the
// suggestion of the next message is (convo_suggest.go).

// convoCard is a card as the screen shows it.
type convoCard struct {
	Tag     string       `json:"tag"`  // "Heads up", "You should know", or "" (another of the plugin's: its own line says it)
	Text    string       `json:"text"` // its words, the screen's wrapping undone
	Choices []cardChoice `json:"choices"`
}

type cardChoice struct {
	Key   string `json:"key"` // a digit, 0–4
	Label string `json:"label"`
}

var (
	cardStar     = regexp.MustCompile(`^\s*\x{2726}\s+(.*)$`)
	cardTag      = regexp.MustCompile(`^(Heads up|You should know)\s+\x{00b7}\s+(.*)$`)
	cardChoices  = regexp.MustCompile(`^\s*([0-4]): \S`)
	cardChoiceAt = regexp.MustCompile(`([0-4]): (.+?)(?:\s{2,}|$)`)
)

// cardOf: the card right above the input box in front, nil where there is
// none. Only blank lines and a note at the right (as "✔ Update installed")
// may stand between it and the box; its key line is its last; its first
// line begins with the star. (So that a card's words quoted in the thread
// are not taken for one.)
func cardOf(screen string) *convoCard {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	box := -1
	for i := len(lines) - 1; i >= 1; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "❯") && pickerRule.MatchString(lines[i-1]) {
			box = i - 1 // (its upper rule)
			break
		}
	}
	if box < 0 {
		return nil
	}
	// Its key line, the first above the box that is not blank nor a note
	// set at the right.
	keys := -1
	for i := box - 1; i >= 0 && i >= box-4; i-- {
		l := lines[i]
		if strings.TrimSpace(l) == "" || rightNote(l) {
			continue
		}
		if cardChoices.MatchString(l) {
			keys = i
		}
		break
	}
	if keys < 0 {
		return nil
	}
	// Its first line: the star, a few lines up (a card is short).
	// (Its lines under the star are set in by two; a line that is not,
	// the thread's, ends the search.)
	top := -1
	for i := keys - 1; i >= 0 && i >= keys-30; i-- {
		l := lines[i]
		if cardStar.MatchString(l) {
			top = i
			break
		}
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "  ") {
			break
		}
	}
	if top < 0 {
		return nil
	}
	c := &convoCard{}
	head := cardStar.FindStringSubmatch(lines[top])[1]
	if m := cardTag.FindStringSubmatch(head); m != nil {
		c.Tag, head = m[1], m[2]
	}
	text := strings.TrimSpace(head)
	for _, l := range lines[top+1 : keys] {
		l = strings.TrimSpace(l)
		if l == "" { // (a paragraph's end)
			if !strings.HasSuffix(text, "\n\n") {
				text = strings.TrimRight(text, "\n") + "\n\n"
			}
			continue
		}
		text = joinWrapped(text, l)
	}
	c.Text = strings.TrimSpace(text)
	for _, m := range cardChoiceAt.FindAllStringSubmatch(strings.TrimSpace(lines[keys]), -1) {
		c.Choices = append(c.Choices, cardChoice{Key: m[1], Label: strings.TrimSpace(m[2])})
	}
	if c.Text == "" || len(c.Choices) == 0 {
		return nil
	}
	return c
}

// rightNote: a line with something only at its far right (a note of Claude
// Code's own, as "✔ Update installed · /restart to apply").
func rightNote(l string) bool {
	t := strings.TrimLeft(l, " ")
	return len(l)-len(t) >= 24 && t != ""
}

// joinWrapped: text and its next line as one, the screen's wrap undone: no
// space between two that are not Latin (a line of Chinese broken anywhere),
// one between words.
func joinWrapped(text, next string) string {
	if text == "" || strings.HasSuffix(text, "\n") {
		return text + next
	}
	a, _ := utf8.DecodeLastRuneInString(text)
	b, _ := utf8.DecodeRuneInString(next)
	if a < 0x2e80 && b < 0x2e80 {
		return text + " " + next
	}
	return text + next
}
