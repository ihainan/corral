package web

import (
	"context"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Commands a Claude Code in a tmux session left running in the background,
// as herdr panes have them told (herdr_bgcmd.go): no watch tells a tmux
// pane's status, so the conversation's reads do (its session file: idle),
// and the host is looked at only while one is read and at rest. One prober
// a tmux session, made at its first such read, ended with the session.
// (tmuxForget: a pane no read has told of for this long is no longer
// looked for: the page closed, or another agent shown.)
const tmuxForget = cmdKept

func (s *Server) tmuxCmdsOf(sess *term.Session) *cmdProber {
	if p, ok := s.tmuxCmds.Load(sess.ID); ok {
		return p.(*cmdProber)
	}
	p := newCmdProber(func(ctx context.Context) (map[string][]bgCmd, bool, error) {
		out, err := sshx.Output(ctx, sess.Client(), cmdScanCommand(), cmdScanMax)
		if err != nil {
			return nil, false, err
		}
		got, whole := parseCmdScan(string(out))
		return got, whole, nil
	})
	p.forget = tmuxForget
	if was, loaded := s.tmuxCmds.LoadOrStore(sess.ID, p); loaded {
		return was.(*cmdProber)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-sess.Finished()
		cancel()
		s.tmuxCmds.Delete(sess.ID)
	}()
	go p.run(ctx)
	return p
}

// tmuxCmdsTell: what the conversation read of a tmux Claude Code (status as
// convoProcStatus says it) is told to its prober; what it left running, if
// that is known.
func (s *Server) tmuxCmdsTell(sess *term.Session, st *convoState) []bgCmd {
	if sess.Persistence != "tmux" || st.agent != "claude" || st.sub || st.bind == nil {
		return nil
	}
	p := s.tmuxCmdsOf(sess)
	p.consider(st.bind.Pane, "claude", st.status, st.bind.Session)
	return p.of(st.bind.Pane, st.bind.Session)
}
