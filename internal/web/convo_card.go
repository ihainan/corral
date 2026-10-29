package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Claude Code's own cards above its input box: its you-should-know plugin
// (a model of its own watching the session) offers something to know, as
// "✦ Heads up · …" or "✦ You should know · …" with keys to answer it
// ("1: Learn more   2: Knew this already   0: Dismiss"), opened out to an
// explanation on 1. They are drawn on the screen only, never in the log, so
// they are read off the screen (with its colours, as the suggestion of the
// next message is: convo_suggest.go). See plans/headsup-card.md.
//
// A key acts only where the input box holds nothing but that digit: it is
// typed into the box, the plugin takes it and empties the box. So a card is
// answered only where the box is empty (a dim suggestion in it counts as
// empty), and a key is pressed only where the card's region of the screen —
// from its head down to the box's lower rule, colours and all — is still
// what was read (cardRegionAwk, on the host, in the command that presses).

// convoCard is a card as the screen shows it.
type convoCard struct {
	State      string       `json:"state"` // offer, explained, making, folded, ready, failed, other
	Tag        string       `json:"tag,omitempty"`
	Text       string       `json:"text"` // its head's words and its body's, the screen's wrapping undone
	Choices    []cardChoice `json:"choices"`
	Actionable bool         `json:"actionable"` // its keys are read whole (not wrapped, not cut, not twice)
	Empty      bool         `json:"empty"`      // the input box under it holds nothing typed
	digest     string       // its region's (see cardRegion)
}

type cardChoice struct {
	Key   string `json:"key"` // a digit
	Label string `json:"label"`
}

var (
	cardHeadSplit = regexp.MustCompile(`(\s{2,}|\s\x{00b7}\s)(\d: )`)
	cardKeyLine   = regexp.MustCompile(`^\s{2,}\d: `)
	cardChoiceAt  = regexp.MustCompile(`(\d): (.+?)(?:\s{2,}|$)`)
	cardTagged    = regexp.MustCompile(`^(Heads up|You should know)\s+\x{00b7}\s+`)
)

// cardOf: the card right above the input box in front, on a screen read
// with its colours; nil where there is none (or it cannot be read).
// cardFrame: where a card is on a screen, before its words are read: the
// screen's lines (and without their colours), the box's upper rule, its
// prompt's line and its lower rule, the card's head. What the host's
// cardRegionAwk does, done as it does it.
type cardFrame struct {
	lines, plain         []string
	top, mark, end, star int
}

func cardFrameOf(screen string) *cardFrame {
	lines := strings.Split(strings.TrimRight(screen, "\r\n"), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = strings.ReplaceAll(sgr.ReplaceAllString(l, ""), "\r", "") // (as cardRegionAwk: every \r)
	}
	// The input box in front: a line beginning with the prompt's mark
	// between two rules, at the screen's foot, nothing chosen among under it.
	// (Every test here that the host's cardRegionAwk makes too is made as
	// it makes it, byte for byte: blanks are ASCII ones, see cardBlank.)
	top, mark, end := -1, -1, -1
	for i := len(plain) - 1; i >= 1; i-- {
		if strings.HasPrefix(strings.TrimLeft(plain[i], cardBlank), "❯") && pickerRule.MatchString(plain[i-1]) {
			top, mark = i-1, i
			break
		}
	}
	if top < 0 {
		return nil
	}
	for j := mark + 1; j < len(plain); j++ {
		if pickerRule.MatchString(plain[j]) {
			end = j
			break
		}
	}
	if end < 0 || len(plain)-end-1 > typedFoot {
		return nil
	}
	for _, l := range plain[end+1:] {
		if strings.Contains(l, "❯") || typedChoice.MatchString(l) {
			return nil
		}
	}
	// Its head: the nearest line above that begins, at its very start, with
	// the star; every line between set in by two at least, or blank, or a
	// note at the far right — a line of the thread's ("● …") ends the search.
	star := -1
	for k := top - 1; k >= 0 && k >= top-60; k-- {
		l := plain[k]
		if strings.HasPrefix(l, "✦ ") {
			star = k
			break
		}
		if strings.Trim(l, cardBlank) != "" && !strings.HasPrefix(l, "  ") && !rightNote(l) {
			return nil
		}
	}
	if star < 0 {
		return nil
	}
	return &cardFrame{lines, plain, top, mark, end, star}
}

func cardOf(screen string) *convoCard {
	f := cardFrameOf(screen)
	if f == nil {
		return nil
	}
	lines, plain, top, mark, end, star := f.lines, f.plain, f.top, f.mark, f.end, f.star
	c := &convoCard{Actionable: true}
	head := strings.TrimSpace(strings.TrimPrefix(plain[star], "✦ "))
	inline := ""
	if m := cardHeadSplit.FindStringSubmatchIndex(head); m != nil {
		head, inline = strings.TrimSpace(head[:m[0]]), head[m[4]:]
	}
	if m := cardTagged.FindStringIndex(head); m != nil {
		c.Tag = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(head[:m[1]]), "·"))
		head = head[m[1]:]
	}
	// Its lines under the head, the blank and the notes at the right at
	// their end let go.
	last := top - 1
	for last > star && (strings.TrimSpace(plain[last]) == "" || rightNote(plain[last])) {
		last--
	}
	// Its keys: from the head where some are on it, else from the first
	// line that begins with one; every line from there one of keys, else
	// they cannot be read whole.
	keysAt := last + 1
	if inline != "" {
		keysAt = star + 1
	} else {
		for j := star + 1; j <= last; j++ {
			if cardKeyLine.MatchString(plain[j]) {
				keysAt = j
				break
			}
		}
	}
	keyText := []string{}
	// (The screen's width: the box's rules are drawn across all of it.)
	width := utf8.RuneCountInString(strings.Trim(plain[top], cardBlank))
	if inline != "" {
		keyText = append(keyText, inline)
		if cellWidth(strings.TrimRight(plain[star], " ")) >= width-1 {
			c.Actionable = false
		}
	}
	for j := keysAt; j <= last; j++ {
		l := plain[j]
		if strings.TrimSpace(l) == "" || rightNote(l) {
			continue
		}
		if !cardKeyLine.MatchString(l) {
			c.Actionable = false // (a key's words wrapped, or words under the keys)
			continue
		}
		keyText = append(keyText, strings.TrimSpace(l))
		if cellWidth(strings.TrimRight(l, " ")) >= width-1 {
			c.Actionable = false // (to the screen's edge: its last key's words may be cut there, with no "…")
		}
	}
	seen := map[string]bool{}
	for _, t := range keyText {
		for _, m := range cardChoiceAt.FindAllStringSubmatch(t, -1) {
			label := strings.TrimSpace(m[2])
			if seen[m[1]] || strings.HasSuffix(label, "…") {
				c.Actionable = false // (a key twice, or its words cut short)
			}
			seen[m[1]] = true
			c.Choices = append(c.Choices, cardChoice{Key: m[1], Label: label})
		}
	}
	// Its words: the head's, then the body's (paragraphs kept).
	text := head
	for j := star + 1; j < keysAt && j <= last; j++ {
		l := strings.TrimSpace(plain[j])
		if l == "" {
			if !strings.HasSuffix(text, "\n\n") {
				text = strings.TrimRight(text, "\n") + "\n\n"
			}
			continue
		}
		if rightNote(plain[j]) {
			continue
		}
		text = joinWrapped(text, l)
	}
	c.Text = strings.TrimSpace(text)
	folded := star > 0 && rightNote(plain[star-1]) && strings.TrimSpace(plain[star-1]) == "[+]"
	c.State = cardState(head, c.Choices, folded && star+1 > last)
	if c.State == "" {
		return nil
	}
	if c.State == "making" || c.State == "folded" {
		c.Choices, c.Actionable = nil, false
	}
	// Whether the box holds anything typed (a dim suggestion is no typing).
	var st sgrState
	for j := 0; j < mark; j++ {
		st = st.through(lines[j])
	}
	c.Empty = true
	for j := mark; j < end; j++ {
		l := strings.TrimRight(lines[j], "\r")
		if j == mark {
			k := strings.Index(l, "❯")
			st = st.through(l[:k])
			l = l[k+len("❯"):]
		}
		_, ok, next := st.greyed(l)
		if !ok {
			c.Empty = false
			break
		}
		st = next
	}
	c.digest = cardRegion(lines, star, end)
	return c
}

// cardState: what a card is doing, by its head and its keys; "" where it is
// not one to show.
func cardState(head string, choices []cardChoice, folded bool) string {
	has := func(labels ...string) bool {
		for _, c := range choices {
			for _, l := range labels {
				if c.Label == l {
					return true
				}
			}
		}
		return false
	}
	switch {
	case strings.HasPrefix(head, "One moment") || strings.HasPrefix(head, "Making your page"):
		return "making"
	case folded:
		return "folded"
	case len(choices) == 0:
		return ""
	case has("Learn more", "Knew this already"):
		return "offer"
	case has("Understood", "Chat in main session"):
		return "explained"
	case has("Helpful", "Not helpful"):
		return "ready"
	case len(choices) == 1 && choices[0].Label == "OK":
		return "failed"
	}
	return "other"
}

// cardRegion: the SHA-256 of a card's region of a screen read with its
// colours: its lines from the head (star) to the box's lower rule (end),
// each as drawn, its trailing spaces let go, each ended by a newline — as
// cardRegionAwk takes it on the host.
func cardRegion(lines []string, star, end int) string {
	var b strings.Builder
	for _, l := range lines[star : end+1] {
		b.WriteString(strings.TrimRight(l, " \t\r"))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// cardRegionAwk takes a card's region from a screen read with its colours
// (on its standard input) as cardRegion does: the input box in front (the
// last line beginning, its colours aside, with the prompt's mark under a
// rule), its lower rule, and the nearest line above it beginning with the
// star at its very start; its lines printed as drawn, trailing spaces let
// go. Nothing printed where there is none. (Bytes, not characters: any awk.)
const cardRegionAwk = `function note(q) { match(q, /^ */); return RLENGTH >= 24 && length(q) > RLENGTH }
{ raw[NR] = $0; p = $0; gsub(/\033\[[0-9;]*m/, "", p); gsub(/\r/, "", p); plain[NR] = p }
END {
  n = NR
  while (n >= 1 && raw[n] ~ /^\r*$/) n--
  rule = "^[ \t\f]*(\342\224\200|\342\224\201)(\342\224\200|\342\224\201)(\342\224\200|\342\224\201)(\342\224\200|\342\224\201)(\342\224\200|\342\224\201)(\342\224\200|\342\224\201)(\342\224\200|\342\224\201)(\342\224\200|\342\224\201)"
  mark = 0
  for (i = n; i >= 2; i--) { q = plain[i]; sub(/^[ \t\f]+/, "", q); if (index(q, "\342\235\257") == 1 && plain[i-1] ~ rule) { mark = i; break } }
  if (!mark) exit
  end = 0
  for (j = mark + 1; j <= n; j++) if (plain[j] ~ rule) { end = j; break }
  if (!end || n - end > 10) exit
  for (j = end + 1; j <= n; j++) if (index(plain[j], "\342\235\257") || plain[j] ~ /^[ \t\f]*[0-9]+\.[ \t\f]/) exit
  star = 0
  for (k = mark - 2; k >= 1 && k >= mark - 61; k--) {
    if (index(plain[k], "\342\234\246 ") == 1) { star = k; break }
    q = plain[k]
    if (q ~ /[^ \t\f]/ && substr(q, 1, 2) != "  " && !note(q)) exit
  }
  if (!star) exit
  for (k = star; k <= end; k++) { l = raw[k]; sub(/[ \t\r]+$/, "", l); print l }
}`

// cellWidth: at most how many of the terminal's cells s takes: a letter of
// ASCII one, any other two (wide ones are; a narrow one, or a mark that
// takes none, counted two only makes a line seem nearer the screen's edge,
// its keys not pressed from here where they could have been).
func cellWidth(s string) int {
	n := 0
	for _, r := range s {
		if r < 0x80 {
			n++
		} else {
			n += 2
		}
	}
	return n
}

// cardBlank: what is blank, here and on the host (awk's [ \t\f]; \r is
// taken out of every line before, \n cannot be in one).
const cardBlank = " \t\f"

// rightNote: a line with something only at its far right (a note of Claude
// Code's own, as "✔ Update installed · /restart to apply", or the card's
// "[-]").
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

// sameCard: the card read now is the one the page shows (its state, its
// words, its keys), as asked to answer.
func sameCard(a, b *convoCard) bool {
	if a == nil || b == nil || a.State != b.State || a.Tag != b.Tag || a.Text != b.Text || len(a.Choices) != len(b.Choices) {
		return false
	}
	for i := range a.Choices {
		if a.Choices[i] != b.Choices[i] {
			return false
		}
	}
	return true
}

// cardPressed: what was pressed last on each pane's card (its words and the
// key) and when: the same again within cardAgain is a double tap, not done.
var cardPressed = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

const cardAgain = 5 * time.Second

// cardPress: a key of the card above the pane's input box pressed, where
// the screen, read now, shows the card the page shows (want), its keys read
// whole, the key among them, the box empty; the key pressed in the same
// command on the host that finds its region of the screen unchanged.
func (s *Server) cardPress(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, key string, want *convoCard, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var held sync.WaitGroup
	held.Add(1)
	defer func() {
		go func() {
			held.Wait()
			release()
		}()
	}()
	refuse := func(code, why string) {
		*outcome, *reason = "refused", code
		writeError(w, http.StatusConflict, code, why)
	}
	res, err := sshx.Exec(ctx, sess.Client(), convoColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: held.Done})
	if err != nil {
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	c := cardOf(string(res.Out))
	switch {
	case c == nil:
		refuse("card_gone", "the card is no longer there")
		return
	case !sameCard(c, want):
		refuse("card_changed", "the card changed: look again")
		return
	case !c.Actionable:
		refuse("card_unreadable", "its keys cannot be read whole: answer it in the terminal")
		return
	case !c.Empty:
		refuse("card_box_typed", "the terminal's input box holds unsent words")
		return
	}
	found := false
	for _, ch := range c.Choices {
		found = found || ch.Key == key
	}
	if !found {
		refuse("card_no_key", "that key is not the card's")
		return
	}
	id := sess.ID + "\x00" + pane + "\x00" + c.Text + "\x00" + key
	cardPressed.Lock()
	if at, ok := cardPressed.m[id]; ok && time.Since(at) < cardAgain {
		cardPressed.Unlock()
		refuse("card_just_pressed", "that key was pressed a moment ago")
		return
	}
	for k, at := range cardPressed.m {
		if time.Since(at) > cardAgain {
			delete(cardPressed.m, k)
		}
	}
	mine := time.Now()
	cardPressed.m[id] = mine
	cardPressed.Unlock()
	// (Refused on the host, nothing pressed: not counted as a press.)
	unmark := func() {
		cardPressed.Lock()
		if at, ok := cardPressed.m[id]; ok && at.Equal(mine) {
			delete(cardPressed.m, id)
		}
		cardPressed.Unlock()
	}
	held.Add(1)
	_, err = sshx.Exec(ctx, sess.Client(), convoGatedCommand("card:"+c.digest, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, replyPress(sess.Persistence, []string{key}), 0), sendOut,
		sshx.ExecOptions{Stdin: strings.NewReader(""), Release: held.Done})
	var exitErr *sshx.ExitError
	switch {
	case err == nil:
		*outcome = "pressed"
		writeJSON(w, http.StatusOK, map[string]any{"pressed": true})
	case errors.Is(err, sshx.ErrBusy), errors.As(err, &exitErr) && exitErr.Status == 7:
		unmark()
		refuse("busy", "the pane is busy with another request: try again")
	case errors.As(err, &exitErr) && exitErr.Status == 10:
		unmark()
		refuse("screen_changed", "the screen changed; look again before answering")
	case errors.As(err, &exitErr) && exitErr.Status == 9:
		unmark()
		refuse("agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		unmark()
		refuse("agent_changed", "that pane no longer runs this agent")
	case errors.As(err, &exitErr) && exitErr.Status == 11:
		unmark()
		refuse("cannot_verify", "the host cannot check the screen (no sha256sum): answer in the terminal")
	case errors.As(err, &exitErr) && exitErr.Status == 4:
		unmark()
		*outcome, *reason = "refused", "send_failed"
		writeError(w, http.StatusBadGateway, "send_failed", "nothing could be sent to the pane")
	default:
		*outcome, *reason = "unknown", "no_answer"
		writeError(w, http.StatusBadGateway, "no_answer", "the host did not answer")
	}
}

// POST /api/sessions/{id}/conversation/card (the same body as a send, with
// the key and the card as the page shows it): that key of the card pressed.
func (s *Server) apiConversationCard(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "card")
}
