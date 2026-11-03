package web

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Where a message went that never turned up in the conversation's log: a
// subagent's, when Claude Code had that subagent's conversation in front
// as it was typed (agent-view.md). Claude Code writes it in that subagent's
// own log (<session>/subagents/agent-<id>.jsonl), as a row of the person's
// (origin "human"), wrapped as told mid-turn. Found there, the page says
// so, for sure, instead of "whether it arrived is not known" for a day.

// sentWent: the subagent that took a message in.
type sentWent struct {
	Agent       string `json:"agent"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

const (
	wentOut      = 2 << 20 // what the host's search prints, at most
	wentFragMax  = 96      // bytes of the message looked for (the longest run of it written as is)
	wentFragMin  = 3
	wentMaxFiles = 100
	wentSkew     = 3 * time.Minute // (the host's clock and the gateway's)
	wentMidTurn  = "The user sent a new message while you were working:\n"
	wentMidEnd   = "\n\nThis is how Claude Code surfaces messages the user sends mid-turn"
)

// wentCommand lists the person's rows (origin "human") of the subagents'
// logs (dir, its subagents/ folder) changed in the last minutes holding
// frag — the person's only first: a short message's words are in many a
// row of a long log, which would fill what is printed — each as
// "./agent-<id>.jsonl:<line>", a few a log, and after the lines of each
// log that had any, its meta as "\001meta ./agent-<id>.jsonl\t<json>".
// Not through links: the folder and the session's folder plain ones, the
// logs plain files (find -type f). Fixed script; the folder (from the
// conversation's located log), the minutes and the fragment its arguments.
func wentCommand(dir string, minutes int, frag string) string {
	script := `d=$1 m=$2 fr=$3
p=${d%/subagents}
[ -d "$p" ] && [ ! -L "$p" ] && [ -d "$d" ] && [ ! -L "$d" ] || exit 0
cd "$d" 2>/dev/null || exit 0
find . -maxdepth 1 -type f -name 'agent-*.jsonl' -mmin "-$m" 2>/dev/null | head -n ` + strconv.Itoa(wentMaxFiles) + ` | while IFS= read -r f; do
  h=$(grep -F -- '"origin":{"kind":"human"' "$f" 2>/dev/null | grep -F -- "$fr" | head -n 50 | sed "s|^|$f:|")
  [ -n "$h" ] || continue
  printf '%s\n' "$h"
  mf=${f%.jsonl}.meta.json
  if [ -f "$mf" ] && [ ! -L "$mf" ]; then printf '\001meta %s\t' "$f"; head -c 8192 "$mf" | tr -d '\n'; printf '\n'; fi
done | head -c ` + strconv.Itoa(wentOut)
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(dir) + " " + strconv.Itoa(minutes) + " " + sshx.QuoteShell(frag)
}

// wentFragment: a piece of text as Claude Code writes it in its log (JSON:
// " \ and the line breaks and tabs escaped, nothing else — sendText lets
// no other control character through): its longest run written as is, at
// most wentFragMax bytes (cut at a character's end); "" where none is long
// enough to look for.
func wentFragment(text string) string {
	best := ""
	// (Claude Code keeps it without the blanks at its ends: review559.)
	for _, run := range strings.FieldsFunc(strings.TrimSpace(text), func(r rune) bool { return r == '"' || r == '\\' || r == '\n' || r == '\t' || r == '\r' }) {
		if len(run) > len(best) {
			best = run
		}
	}
	if len(best) > wentFragMax {
		cut := wentFragMax
		for cut > 0 && !utf8RuneStart(best[cut]) {
			cut--
		}
		best = best[:cut]
	}
	if len(best) < wentFragMin {
		return "" // (the person's rows all looked through: few)
	}
	return best
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// wentNorm: a text as compared: its words, one space between.
func wentNorm(s string) string { return strings.Join(strings.Fields(s), " ") }

// wentOf reads the search's output: the subagent a row of the person's
// holding text belongs to, written at from or after (the latest such row),
// nil where none is. Not that one, but the same words said to it before:
// its row's own time tells them apart, not its log's (review559).
func wentOf(out []byte, text string, from time.Time) *sentWent {
	want := wentNorm(text)
	if want == "" {
		return nil
	}
	var at time.Time
	metas := map[string]sentWent{}
	var hit string
	for _, l := range bytes.Split(out, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(l, []byte("\001meta ./agent-")); ok {
			name, js, ok := bytes.Cut(rest, []byte("\t"))
			id, isLog := strings.CutSuffix(string(name), ".jsonl")
			if !ok || !isLog || !convo.SubagentID(id) {
				continue
			}
			var m struct {
				AgentType   string `json:"agentType"`
				Description string `json:"description"`
			}
			if json.Unmarshal(js, &m) == nil {
				metas[id] = sentWent{Agent: id, Type: clip(m.AgentType, 64), Description: clip(m.Description, 120)}
			}
			continue
		}
		rest, ok := bytes.CutPrefix(l, []byte("./agent-"))
		if !ok {
			continue
		}
		name, row, ok := bytes.Cut(rest, []byte(".jsonl:"))
		if !ok || !convo.SubagentID(string(name)) {
			continue
		}
		if said, when, ok := personSaid(row); ok && wentNorm(said) == want && !when.Before(from) && !when.Before(at) {
			hit, at = string(name), when
		}
	}
	if hit == "" {
		return nil
	}
	if m, ok := metas[hit]; ok {
		return &m
	}
	return &sentWent{Agent: hit}
}

// personSaid: what a row of a subagent's log has the person say — a row of
// theirs (type user, origin human), its text, unwrapped where it was told
// mid-turn.
func personSaid(row []byte) (string, time.Time, bool) {
	var r struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Origin    struct {
			Kind string `json:"kind"`
		} `json:"origin"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(row, &r) != nil || r.Type != "user" || r.Origin.Kind != "human" || r.Message.Role != "user" || r.Timestamp.IsZero() {
		return "", time.Time{}, false
	}
	var said string
	if json.Unmarshal(r.Message.Content, &said) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(r.Message.Content, &parts) != nil {
			return "", time.Time{}, false
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		said = b.String()
	}
	if s, ok := strings.CutPrefix(said, wentMidTurn); ok {
		if i := strings.LastIndex(s, wentMidEnd); i >= 0 {
			s = s[:i]
		}
		said = s
	}
	return said, r.Timestamp, true
}

// sentWentTo looks for the message in the subagents' logs of the
// conversation read for this pane (key) — only while that reading is of the
// Claude Code session sid — written since it was sent: ago, how long before
// now (ms, by the page's own clock: what it says of the time of day is not
// taken, a browser's may be off — review559), a few minutes allowed for
// the host's; over two days, not looked for.
func (s *Server) sentWentTo(ctx context.Context, sess *term.Session, key, sid, text string, ago int64) *sentWent {
	st := s.convos.peek(key)
	if st == nil {
		return nil
	}
	st.mu.Lock()
	root, agent, reason, logSID, sub := st.log, st.agent, st.reason, st.sid, st.sub
	st.mu.Unlock()
	if root == "" || reason != "" || agent != "claude" || sub || logSID != sid || !strings.HasSuffix(root, "/"+sid+".jsonl") || !strings.Contains(root, "/.claude/projects/") {
		return nil
	}
	frag := wentFragment(text)
	if frag == "" {
		return nil
	}
	since := time.Duration(ago) * time.Millisecond
	if ago < 0 || since > 2*24*time.Hour {
		return nil
	}
	from := time.Now().Add(-since - wentSkew)
	minutes := int((since+wentSkew)/time.Minute) + 2
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), wentCommand(strings.TrimSuffix(root, ".jsonl")+"/subagents", minutes, frag), wentOut+(64<<10), sshx.ExecOptions{})
	if err != nil {
		return nil
	}
	return wentOf(res.Out, text, from)
}
