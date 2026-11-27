package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
)

// GET /api/sessions/{id}/pane-inspect?pane=<pane>
// Returns live foreground process information, Herdr agent detection rationale
// (agent.explain), and Git worktree context for a Herdr or tmux pane.
// Read-only; called on demand when the user opens the Session Info Card.

type paneInspectProc struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	Cmdline string `json:"cmdline,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
}

type paneInspectRule struct {
	ID     string `json:"id"`
	State  string `json:"state,omitempty"`
	Region string `json:"region,omitempty"`
}

type paneInspectExplain struct {
	Agent         string            `json:"agent,omitempty"`
	Authority     string            `json:"authority,omitempty"`
	ScreenSkipped bool              `json:"screen_skipped,omitempty"`
	SkipReason    string            `json:"skip_reason,omitempty"`
	MatchedRules  []paneInspectRule `json:"matched_rules,omitempty"`
}

type paneInspectWorktree struct {
	Linked   bool   `json:"linked"`
	Branch   string `json:"branch,omitempty"`
	Path     string `json:"path,omitempty"`
	RepoName string `json:"repo_name,omitempty"`
	Total    int    `json:"total,omitempty"`
}

type paneInspectResponse struct {
	Mode      string               `json:"mode"`
	Pane      string               `json:"pane,omitempty"`
	ShellPID  int                  `json:"shell_pid,omitempty"`
	Zoomed    bool                 `json:"zoomed,omitempty"`
	Processes []paneInspectProc    `json:"processes,omitempty"`
	Explain   *paneInspectExplain  `json:"explain,omitempty"`
	Worktree  *paneInspectWorktree `json:"worktree,omitempty"`
}

func parseHerdrExplain(raw json.RawMessage) *paneInspectExplain {
	if len(raw) == 0 {
		return nil
	}
	var doc struct {
		Agent                 string `json:"agent"`
		ScreenSkipped         bool   `json:"screen_detection_skipped"`
		SkipReason            string `json:"screen_detection_skip_reason"`
		FullLifecycleHookAuth bool   `json:"full_lifecycle_hook_authority"`
		EvaluatedRules        []struct {
			ID      string `json:"id"`
			Matched bool   `json:"matched"`
			State   string `json:"state"`
			Region  string `json:"region"`
		} `json:"evaluated_rules"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := &paneInspectExplain{
		Agent:         truncate(doc.Agent, 64),
		ScreenSkipped: doc.ScreenSkipped,
		SkipReason:    truncate(doc.SkipReason, 120),
	}
	if doc.FullLifecycleHookAuth {
		out.Authority = "hook"
	} else {
		out.Authority = "screen"
	}
	for _, r := range doc.EvaluatedRules {
		if !r.Matched || r.ID == "" {
			continue
		}
		out.MatchedRules = append(out.MatchedRules, paneInspectRule{
			ID:     truncate(r.ID, 64),
			State:  truncate(r.State, 32),
			Region: truncate(r.Region, 64),
		})
		if len(out.MatchedRules) >= 6 {
			break
		}
	}
	if out.Agent == "" && len(out.MatchedRules) == 0 && !out.ScreenSkipped {
		return nil
	}
	return out
}

const tmuxPaneInspectScript = `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
sess=$1 pane=$2
if [ -n "$pane" ]; then
  line=$(tmux -u display-message -p -t "$pane" '#{pane_id}|#{pane_pid}|#{pane_tty}|#{pane_current_command}|#{window_zoomed_flag}' 2>/dev/null)
else
  line=$(tmux -u display-message -p -t "=$sess:" '#{pane_id}|#{pane_pid}|#{pane_tty}|#{pane_current_command}|#{window_zoomed_flag}' 2>/dev/null)
fi
[ -n "$line" ] || exit 0
printf 'pane\t%s\n' "$line"
tty=$(printf '%s' "$line" | cut -d'|' -f3)
[ -n "$tty" ] && [ -r /proc/self/stat ] || exit 0
tdev=${tty#/dev/}
ps -t "$tdev" -o pid=,comm=,args= 2>/dev/null | head -n 16 | while IFS= read -r row; do
  printf 'proc\t%s\n' "$row"
done`

func tmuxPaneInspectCommand(tmuxSession, pane string) string {
	return "sh -c " + sshx.QuoteShell(tmuxPaneInspectScript) + " corral " + sshx.QuoteShell(tmuxSession) + " " + sshx.QuoteShell(pane)
}

func parseTmuxPaneInspect(out []byte) paneInspectResponse {
	res := paneInspectResponse{Mode: "tmux"}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		kind, rest, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch kind {
		case "pane":
			parts := strings.Split(rest, "|")
			if len(parts) >= 1 {
				res.Pane = parts[0]
			}
			if len(parts) >= 2 {
				res.ShellPID, _ = strconv.Atoi(parts[1])
			}
			if len(parts) >= 5 {
				res.Zoomed = parts[4] == "1"
			}
		case "proc":
			fields := strings.Fields(rest)
			if len(fields) < 2 {
				continue
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil || pid <= 0 {
				continue
			}
			name := fields[1]
			cmdline := strings.Join(fields[2:], " ")
			if cmdline == "" {
				cmdline = name
			}
			res.Processes = append(res.Processes, paneInspectProc{
				PID:     pid,
				Name:    truncate(name, 64),
				Cmdline: truncate(cmdline, 240),
			})
			if len(res.Processes) >= 16 {
				return res
			}
		}
	}
	return res
}

func (s *Server) apiPaneInspect(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	pane := strings.TrimSpace(r.URL.Query().Get("pane"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch sess.Persistence {
	case "herdr":
		if !herdrPaneID.MatchString(pane) {
			writeError(w, http.StatusBadRequest, "bad_request", "valid pane id is required")
			return
		}
		socket := s.herdrSocks.get(sess.ID)
		if socket == "" {
			writeError(w, http.StatusConflict, "herdr_unavailable", "herdr is not available in this session")
			return
		}
		out := paneInspectResponse{Mode: "herdr", Pane: pane}
		var paneWS, paneCwd string
		if hp, ok := s.herdrSocks.paneOf(sess.ID, pane); ok {
			paneWS = hp.WorkspaceID
			paneCwd = hp.Cwd
		}

		// Dial helper (herdr closes connections on RPC errors such as not_git_worktree).
		withClient := func(fn func(*herdr.Client) error) {
			cl, err := herdr.DialWait(ctx, sess.Client(), socket, 4*time.Second)
			if err != nil {
				return
			}
			defer cl.Close()
			_ = fn(cl)
		}

		withClient(func(cl *herdr.Client) error {
			pi, err := cl.PaneProcessInfo(ctx, pane)
			if err != nil || pi == nil {
				return err
			}
			out.ShellPID = pi.ShellPID
			for _, pr := range pi.ForegroundProcesses {
				cmd := strings.TrimSpace(pr.Cmdline)
				if cmd == "" && len(pr.Argv) > 0 {
					cmd = strings.Join(pr.Argv, " ")
				}
				out.Processes = append(out.Processes, paneInspectProc{
					PID:     pr.PID,
					Name:    truncate(pr.Name, 64),
					Cmdline: truncate(cmd, 240),
					Cwd:     truncate(pr.Cwd, 200),
				})
				if len(out.Processes) >= 16 {
					break
				}
			}
			return nil
		})

		withClient(func(cl *herdr.Client) error {
			raw, err := cl.AgentExplain(ctx, pane)
			if err != nil {
				return err
			}
			out.Explain = parseHerdrExplain(raw)
			return nil
		})

		if paneWS != "" || paneCwd != "" {
			withClient(func(cl *herdr.Client) error {
				wl, err := cl.ListWorktrees(ctx, paneCwd, paneWS)
				if err != nil || wl == nil || len(wl.Worktrees) == 0 {
					return err
				}
				wt := &paneInspectWorktree{
					RepoName: truncate(wl.Source.RepoName, 80),
					Total:    len(wl.Worktrees),
				}
				for _, item := range wl.Worktrees {
					if (paneWS != "" && item.OpenWorkspaceID == paneWS) || (paneCwd != "" && item.Path == paneCwd) {
						wt.Linked = item.IsLinkedWorktree
						wt.Branch = truncate(item.Branch, 80)
						wt.Path = truncate(item.Path, 200)
						break
					}
				}
				out.Worktree = wt
				return nil
			})
		}

		writeJSON(w, http.StatusOK, out)

	case "tmux":
		if pane != "" && !tmuxPaneID.MatchString(pane) {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid tmux pane id")
			return
		}
		res, err := sshx.Exec(ctx, sess.Client(), tmuxPaneInspectCommand(sess.TmuxSession, pane), 64<<10, sshx.ExecOptions{Interactive: true, AwaitSlot: 5 * time.Second})
		if err != nil {
			writeError(w, http.StatusBadGateway, "inspect_failed", "could not inspect tmux pane")
			return
		}
		writeJSON(w, http.StatusOK, parseTmuxPaneInspect(res.Out))

	default:
		writeJSON(w, http.StatusOK, paneInspectResponse{Mode: sess.Persistence})
	}
}
