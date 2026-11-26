package web

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// What an agent said last, for its "finished" alert (doneBody).
//
// Claude Code: the end of its log is read once at its turn's end already
// (bgProber); what it said last comes with that read. Codex: its log (the
// thread's rollout) is read for it, its end only, once, as the alert is
// about to go (no one looking at the pane): where the conversation's own
// reading has its log, that one; else where a short look at the host's
// Codexes tells the pane's for sure (codexPaneSure). Codex writes, as a
// turn ends, what it said last (task_complete's last_agent_message).

const (
	saidCodexWait = 2 * time.Second // (all of it: finding the log, reading its end)
	saidCodexTail = 64 << 10
	saidAgyTail   = 256 << 10 // (its log carries tool results whole: a turn's end further back)
)

// Antigravity CLI (11-26, its alert said only its command line): its
// transcript, where the conversation's own reading has it, its end read
// as Codex's is, the last of its words since the person's last message.
// Where no page has read it (none open, the gateway started since) none
// is looked for: the alert says where, as before.

// lastAnswer: what tr's agent said last in the turn of round, its
// whitespace made single, at most maxSaid letters; "" where not known (not
// read in time, not found for sure, nothing said since the person's
// message). After bgAwait.
func (s *Server) lastAnswer(sess *term.Session, tr herdr.Transition, round int64) string {
	if p := s.herdrSocks.bgOf(sess.ID); p != nil {
		if t := p.said(tr.Pane.ID, round); t != "" {
			return t
		}
	}
	if a := strings.ToLower(tr.Pane.Agent); a == "agy" || strings.Contains(a, "antigravity") {
		return s.agyLastAnswer(sess, tr.Pane)
	}
	if !strings.Contains(strings.ToLower(tr.Pane.Agent), "codex") {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), saidCodexWait)
	defer cancel()
	log := s.codexLogOfPane(ctx, sess, tr.Pane)
	if log == "" {
		return ""
	}
	out, err := sshx.Output(ctx, sess.Client(), codexTailCommand(log, saidCodexTail), saidCodexTail+1024)
	if err != nil {
		return ""
	}
	return codexLastSaid(out)
}

// codexLogOfPane: the log of the Codex thread pane is at, where that is
// known for sure ("" else).
func (s *Server) codexLogOfPane(ctx context.Context, sess *term.Session, pane herdr.Pane) string {
	if st := s.convos.peek(sess.ID + "\x00" + pane.ID); st != nil {
		st.mu.Lock()
		log, agent, reason, sub := st.log, st.agent, st.reason, st.sub
		st.mu.Unlock()
		if agent == "codex" && reason == "" && !sub && codexLogName.MatchString(log) {
			return log
		}
	}
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" {
		return ""
	}
	panes, err := s.codexPanes.of(ctx, sess, socket)
	if err != nil {
		return ""
	}
	if c, ok := codexPaneSure(panes, pane.ID, pane); ok && codexLogName.MatchString(c.Path) {
		return c.Path
	}
	return ""
}

// codexTailCommand prints the end of a Codex log: its last n bytes. Fixed
// command; the size a constant, the log one the host's look or the
// conversation's located (an absolute rollout path, codexLogName), quoted.
func codexTailCommand(log string, n int) string {
	return "tail -c " + strconv.Itoa(n) + " -- " + sshx.QuoteShell(log)
}

// codexLastSaid: what Codex said last, read back from its log's end: the
// last turn's end (task_complete) with words; before that, its last
// message; none of either since the person's last message, "".
func codexLastSaid(tail []byte) string {
	lines := bytes.Split(tail, []byte("\n"))
	var said string
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if len(l) == 0 || !bytes.Contains(l, []byte(`"payload"`)) {
			continue
		}
		var r struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Last    string `json:"last_agent_message"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(l, &r) != nil { // (the first, cut by the tail's start)
			continue
		}
		p := r.Payload
		switch {
		case r.Type == "event_msg" && p.Type == "task_complete" && strings.TrimSpace(p.Last) != "":
			return oneLineSaid(p.Last)
		case r.Type == "event_msg" && p.Type == "user_message":
			return said
		case r.Type == "response_item" && p.Type == "message" && p.Role == "assistant" && said == "":
			var parts []string
			for _, c := range p.Content {
				if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
					parts = append(parts, c.Text)
				}
			}
			if len(parts) > 0 {
				said = oneLineSaid(strings.Join(parts, "\n"))
			}
		}
	}
	return said
}

// oneLineSaid: words with their whitespace made single, at most maxSaid letters.
func oneLineSaid(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > maxSaid {
		t = string(r[:maxSaid])
	}
	return t
}

// agyLastAnswer: what an Antigravity CLI said last, from the end of the
// transcript its conversation reads ("" where that is not had, not read
// in time, or nothing said since the person's last message).
func (s *Server) agyLastAnswer(sess *term.Session, pane herdr.Pane) string {
	st := s.convos.peek(sess.ID + "\x00" + pane.ID)
	if st == nil {
		return ""
	}
	st.mu.Lock()
	log, sid, agent, reason, sub := st.log, st.sid, st.agent, st.reason, st.sub
	st.mu.Unlock()
	l, ok := cleanRemotePath(log)
	if agent != "agy" || reason != "" || sub || !ok || !convoLogOf("agy", l, sid) {
		return ""
	}
	// (The pane's another conversation's now — herdr says whose it runs —: not this one's words. Review b5-2.)
	if as := pane.AgentSession; as != nil && as.Value != "" && !strings.Contains(as.Value, sid) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), saidCodexWait)
	defer cancel()
	out, err := sshx.Output(ctx, sess.Client(), agyTailCommand(l, saidAgyTail), saidAgyTail+1024)
	if err != nil {
		return ""
	}
	return agyLastSaid(out)
}

// agyTailCommand prints the end of an Antigravity transcript — its last n
// bytes — only where it was written in the last few minutes: one its
// conversation read a while ago, the pane's since gone on to another, says
// nothing of the turn just ended (review b5-2). Fixed command; the size a
// constant, the transcript the one its conversation located (absolute,
// convoLogOf), quoted.
func agyTailCommand(log string, n int) string {
	q := sshx.QuoteShell(log)
	return "[ -n \"$(find " + q + " -maxdepth 0 -mmin -" + strconv.Itoa(agyWrittenWithin) + " 2>/dev/null)\" ] && tail -c " + strconv.Itoa(n) + " -- " + q
}

// (A turn's end is written to the transcript as it ends, the alert sent as
// soon as herdr says so: two minutes are room enough, and too short for the
// pane to have gone on to another conversation the cache has not caught up
// with — the one case where its words are not this turn's. Review b6-1.)
const agyWrittenWithin = 2 // minutes

// agyLastSaid: of a transcript's end, the agent's last words since the
// person's last message ("" none). The first line, cut by the tail's
// start, is skipped by the parser.
func agyLastSaid(tail []byte) string {
	p := convo.NewAgy()
	p.Feed(tail)
	es := p.Since(0)
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i]
		if e.Gone {
			continue
		}
		if e.Role == convo.RoleUser {
			return ""
		}
		if e.Role != convo.RoleAssistant {
			continue
		}
		for j := len(e.Parts) - 1; j >= 0; j-- {
			if x := e.Parts[j]; x.Kind == convo.KindText && x.Tool == nil && strings.TrimSpace(x.Text) != "" {
				return oneLineSaid(x.Text)
			}
		}
	}
	return ""
}
