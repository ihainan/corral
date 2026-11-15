package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// A pane a phone's terminal page once set stays that narrow for whoever
// reads it after, the agents page among them (which sets no size at all):
// at 47 columns by 34 rows Claude Code's model picker has its keys line cut
// off and rows with it. Measured (2026-11-10, Claude Code 2.1.321 under
// herdr 0.8.0 and 0.9.5): a terminal made wide redraws within 0.1 s, the
// picker then whole; made narrow again within 0.5 s, its history as it was.
// So the picker is read with the terminal held roomy, and put back after.
const (
	roomyCols, roomyRows   = 160, 50 // the terminal while it is read
	narrowCols, narrowRows = 100, 40 // a pane under either is held roomy
	roomySettle            = 300 * time.Millisecond
)

// paneLayoutCommand prints herdr's layout of the pane's tab. Fixed script;
// the validated pane and the herdr socket are its arguments.
func paneLayoutCommand(pane, socket string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
HERDR_SOCKET_PATH=$2 herdr pane layout --pane "$1" 2>/dev/null 9>&-`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(socket)
}

type rect struct{ Width, Height int }

// paneLayout: the pane's own size and that of the area its tab has, off
// herdr's answer; ok only where both are there.
func paneLayout(out []byte, pane string) (own, area rect, ok bool) {
	var doc struct {
		Result struct {
			Layout struct {
				Area  rect `json:"area"`
				Panes []struct {
					PaneID string `json:"pane_id"`
					Rect   rect   `json:"rect"`
				} `json:"panes"`
			} `json:"layout"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return
	}
	area = doc.Result.Layout.Area
	for _, p := range doc.Result.Layout.Panes {
		if p.PaneID == pane {
			own = p.Rect
		}
	}
	return own, area, own.Width > 0 && own.Height > 0 && area.Width > 0 && area.Height > 0
}

// roomyWanted: whether a pane of that layout is read with the terminal
// held roomy, the session's own terminal being cols by rows. Only a narrow
// or short pane; and only one whose area fits the session's terminal, which
// is what herdr gives it while this session's client is the one it sizes
// by. An area larger than that is another client's (a desktop's herdr on
// the same session): changing this one's size would take the size from it,
// and putting it back would leave the pane at the phone's.
func roomyWanted(own, area rect, cols, rows int) bool {
	if own.Width >= narrowCols && own.Height >= narrowRows {
		return false
	}
	return area.Width <= cols && area.Height <= rows
}

// roomyPane holds the session's terminal roomy for a read of that pane,
// where roomyWanted says so (herdr sessions: the layout is herdr's), and
// waits for the redraw. back puts it back; it is safe to call when nothing
// was done, and more than once.
func (s *Server) roomyPane(ctx context.Context, sess *term.Session, pane, socket string) (back func()) {
	back = func() {}
	cols, rows := sess.Size()
	if sess.Persistence != "herdr" || (cols >= roomyCols && rows >= roomyRows) {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := sshx.Exec(lctx, sess.Client(), paneLayoutCommand(pane, socket), 64<<10, sshx.ExecOptions{})
	if err != nil {
		return
	}
	own, area, ok := paneLayout(res.Out, pane)
	if !ok || !roomyWanted(own, area, cols, rows) {
		return
	}
	b, ok := sess.Roomy(roomyCols, roomyRows)
	if !ok {
		return
	}
	slog.Info("picker: terminal held roomy for the read", "session", sess.ID, "pane", pane, "pane_cols", own.Width, "pane_rows", own.Height)
	select {
	case <-time.After(roomySettle):
	case <-ctx.Done():
	}
	return b
}
