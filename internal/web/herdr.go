package web

// herdr integration: for sessions attached to herdr, the gateway watches the
// herdr server over the same SSH connection and pushes a compact summary of
// workspaces and agents to the browser alongside the terminal stream.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

type herdrAgent struct {
	PaneID string `json:"pane_id"`
	Agent  string `json:"agent"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Cwd    string `json:"cwd"`
}

type herdrWorkspace struct {
	ID      string       `json:"id"`
	Label   string       `json:"label"`
	Number  int          `json:"number"`
	Status  string       `json:"status"`
	Focused bool         `json:"focused"`
	Agents  []herdrAgent `json:"agents"`
}

type herdrSummary struct {
	T          string           `json:"t"`
	Session    string           `json:"session"`
	Version    string           `json:"version"`
	Workspaces []herdrWorkspace `json:"workspaces"`
	Waiting    int              `json:"waiting"` // agents blocked on the user
}

func summarize(session string, s *herdr.Snapshot) herdrSummary {
	out := herdrSummary{T: "herdr", Session: session, Version: s.Version, Workspaces: []herdrWorkspace{}}
	idx := map[string]int{}
	for _, w := range s.Workspaces {
		idx[w.ID] = len(out.Workspaces)
		out.Workspaces = append(out.Workspaces, herdrWorkspace{ID: w.ID, Label: w.Label, Number: w.Number,
			Status: w.AgentStatus, Focused: w.Focused || w.ID == s.FocusedWorkspace, Agents: []herdrAgent{}})
	}
	agents := s.Agents
	if len(agents) == 0 {
		for _, p := range s.Panes {
			if p.Agent != "" {
				agents = append(agents, p)
			}
		}
	}
	for _, p := range agents {
		i, ok := idx[p.WorkspaceID]
		if !ok {
			continue
		}
		out.Workspaces[i].Agents = append(out.Workspaces[i].Agents,
			herdrAgent{PaneID: p.ID, Agent: p.Agent, Status: p.AgentStatus, Title: truncate(p.Title, 120), Cwd: p.Cwd})
		if p.AgentStatus == herdr.StatusBlocked {
			out.Waiting++
		}
	}
	return out
}

// herdrSockets remembers which herdr socket each terminal session is watching.
type herdrSockets struct {
	mu   sync.Mutex
	path map[string]string
}

func (h *herdrSockets) set(id, path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path == "" {
		delete(h.path, id)
	} else {
		h.path[id] = path
	}
}

func (h *herdrSockets) get(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path[id]
}

// watchHerdr runs for the life of a herdr-attached session.
func (s *Server) watchHerdr(sess *term.Session, h *store.Host) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sess.Finished()
		cancel()
	}()
	defer s.herdrSocks.set(sess.ID, "")

	// herdr starts its server when the session attaches; give it a moment.
	var socket string
	for attempt := 0; attempt < 10 && ctx.Err() == nil; attempt++ {
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		list, err := herdr.Discover(dctx, sess.Client())
		dcancel()
		if errors.Is(err, herdr.ErrNotInstalled) {
			return
		}
		for _, si := range list {
			if si.Running && ((h.HerdrSession == "" && si.Default) || si.Name == h.HerdrSession) {
				socket = si.SocketPath
			}
		}
		if socket != "" {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if socket == "" {
		return
	}
	s.herdrSocks.set(sess.ID, socket)
	name := h.HerdrSession
	if name == "" {
		name = "default"
	}
	w := herdr.NewWatcher(sess.Client(), socket,
		func(snap *herdr.Snapshot) {
			if b, err := json.Marshal(summarize(name, snap)); err == nil {
				sess.PublishMeta(b)
			}
		},
		func(tr herdr.Transition) { s.onAgentTransition(sess, h, tr) },
	)
	w.Run(ctx)
}

func (s *Server) onAgentTransition(sess *term.Session, h *store.Host, tr herdr.Transition) {
	s.onHerdrTransition(sess, h, tr)
}

// POST /api/sessions/{id}/herdr {"action":"focus_workspace"|"focus_pane","target":"…"}
func (s *Server) apiHerdrAction(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sess, err := s.terms.Get(u.ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" || sess.State() != term.StateRunning {
		writeError(w, http.StatusConflict, "herdr_unavailable", "herdr is not available in this session")
		return
	}
	var b struct {
		Action string `json:"action"`
		Target string `json:"target"`
	}
	if err := readJSON(r, &b); err != nil || b.Target == "" || len(b.Target) > 64 {
		writeError(w, http.StatusBadRequest, "bad_request", "action and target are required")
		return
	}
	c, err := herdr.Dial(sess.Client(), socket)
	if err != nil {
		writeError(w, http.StatusBadGateway, "herdr_unavailable", "cannot reach herdr")
		return
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	switch b.Action {
	case "focus_workspace":
		err = c.FocusWorkspace(ctx, b.Target)
	case "focus_pane":
		err = c.FocusPane(ctx, b.Target)
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "unknown action")
		return
	}
	if err != nil {
		var apiErr *herdr.APIError
		if errors.As(err, &apiErr) {
			writeError(w, http.StatusUnprocessableEntity, "herdr_error", apiErr.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "herdr_unavailable", "cannot reach herdr")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
