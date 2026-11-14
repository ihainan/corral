package web

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

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
)

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
