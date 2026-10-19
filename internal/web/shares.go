package web

// Read-only watching: a session's owner lets named users watch it. Leave is
// kept with the session (it ends with it) and can be taken back at any
// time, which closes what the user has open. Watchers see the screen,
// recent output included, and nothing else: their input, sizes and
// visibility are ignored (see serveTerminal), and the session's other
// features (files, herdr, ports) stay the owner's.

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

type shareView struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Watching    bool   `json:"watching"` // has the session open now
}

func (s *Server) ownSession(w http.ResponseWriter, r *http.Request) (*term.Session, bool) {
	sess, err := s.terms.Get(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return nil, false
	}
	return sess, true
}

func (s *Server) apiListShares(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.ownSession(w, r)
	if !ok {
		return
	}
	out := []shareView{}
	for uid, n := range sess.Watchers() {
		v := shareView{UserID: uid, Watching: n > 0}
		if u, err := s.store.UserByID(r.Context(), uid); err == nil {
			v.Username, v.DisplayName = u.Username, u.DisplayName
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiAddShare(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.ownSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Username string `json:"username"`
	}
	if err := readJSON(r, &b); err != nil || strings.TrimSpace(b.Username) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "username is required")
		return
	}
	if sess.State() != term.StateRunning {
		writeError(w, http.StatusConflict, "session_ended", "this session has ended")
		return
	}
	me := currentUser(r)
	u, err := s.store.UserByName(r.Context(), strings.TrimSpace(b.Username))
	// A disabled account is answered like a missing one.
	if errors.Is(err, store.ErrNotFound) || (err == nil && u.Disabled) {
		writeError(w, http.StatusNotFound, "no_such_user", "no such user")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if u.ID == me.ID {
		writeError(w, http.StatusBadRequest, "share_self", "the session is yours already")
		return
	}
	sess.Grant(u.ID)
	s.audit(r, "session.share", sess.HostName, map[string]any{"session": sess.ID, "with": u.Username})
	writeJSON(w, http.StatusOK, shareView{UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
}

func (s *Server) apiRemoveShare(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.ownSession(w, r)
	if !ok {
		return
	}
	uid, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	name := ""
	if u, err := s.store.UserByID(r.Context(), uid); err == nil {
		name = u.Username
	}
	sess.Revoke(uid)
	s.audit(r, "session.unshare", sess.HostName, map[string]any{"session": sess.ID, "with": name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type sharedView struct {
	ID        string `json:"id"`
	HostName  string `json:"host_name"`
	OwnerName string `json:"owner_name"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	Title     string `json:"title"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

func (s *Server) viewShared(r *http.Request, sess *term.Session) sharedView {
	info := sess.Info()
	owner := ""
	if u, err := s.store.UserByID(r.Context(), sess.OwnerID); err == nil {
		owner = u.DisplayName
		if owner == "" {
			owner = u.Username
		}
	}
	return sharedView{ID: info.ID, HostName: info.HostName, OwnerName: owner, State: info.State, CreatedAt: info.CreatedAt,
		Title: info.Title, Cols: info.Cols, Rows: info.Rows}
}

// apiListShared lists the sessions others let this user watch.
func (s *Server) apiListShared(w http.ResponseWriter, r *http.Request) {
	out := []sharedView{}
	for _, sess := range s.terms.SharedWith(currentUser(r).ID) {
		out = append(out, s.viewShared(r, sess))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiGetShared(w http.ResponseWriter, r *http.Request) {
	sess, err := s.terms.GetShared(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	writeJSON(w, http.StatusOK, s.viewShared(r, sess))
}
