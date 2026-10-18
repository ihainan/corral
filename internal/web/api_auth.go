package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/store"
)

var Version = "dev"

type meView struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func viewMe(u *store.User) *meView {
	return &meView{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role}
}

func (s *Server) apiBootstrap(w http.ResponseWriter, r *http.Request) {
	needed, err := s.auth.SetupNeeded(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := map[string]any{
		"version":          Version,
		"setup_needed":     needed,
		"default_language": s.cfg.DefaultLanguage,
		"forwarding":       s.cfg.ForwardDomain != "",
		"upload_max_bytes": s.cfg.UploadMaxBytes,
		"user":             nil,
	}
	if u := currentUser(r); u != nil {
		out["user"] = viewMe(u)
	}
	writeJSON(w, http.StatusOK, out)
}

type credentialsBody struct {
	Token       string `json:"token"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request) {
	if !s.auth.AllowAttempt(s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
		return
	}
	var b credentialsBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u, err := s.auth.CompleteSetup(r.Context(), b.Token, strings.TrimSpace(b.Username), strings.TrimSpace(b.DisplayName), b.Password)
	if err != nil {
		s.writeAccountError(w, r, err)
		return
	}
	s.auditAs(r, u, "setup.complete", u.Username, nil)
	s.signIn(w, r, u)
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ip := s.clientIP(r)
	u, tok, err := s.auth.Login(r.Context(), b.Username, b.Password, ip, r.UserAgent())
	if err != nil {
		var locked *auth.LockedError
		switch {
		case errors.As(err, &locked):
			s.auditAs(r, nil, "login.locked", b.Username, nil)
			writeErrorExtra(w, http.StatusTooManyRequests, "locked", "too many failed attempts",
				map[string]any{"retry_after": int(locked.RetryAfter.Seconds())})
		case errors.Is(err, auth.ErrRateLimited):
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
		case errors.Is(err, auth.ErrBusy):
			writeError(w, http.StatusServiceUnavailable, "busy", "the server is busy, try again in a moment")
		case errors.Is(err, auth.ErrInvalidLogin):
			s.auditAs(r, nil, "login.failed", truncate(b.Username, 64), nil)
			writeError(w, http.StatusUnauthorized, "invalid_login", "wrong username or password")
		default:
			s.internalError(w, r, err)
		}
		return
	}
	s.auditAs(r, u, "login.success", u.Username, nil)
	s.setSessionCookie(w, tok)
	writeJSON(w, http.StatusOK, map[string]any{"user": viewMe(u)})
}

func (s *Server) signIn(w http.ResponseWriter, r *http.Request, u *store.User) {
	tok, err := s.auth.StartSession(r.Context(), u.ID, s.clientIP(r), r.UserAgent())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.setSessionCookie(w, tok)
	writeJSON(w, http.StatusOK, map[string]any{"user": viewMe(u)})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	hash := loginHash(r)
	_ = s.auth.Logout(r.Context(), hash)
	s.closeSockets(currentUser(r).ID, hash)
	s.audit(r, "logout", "", nil)
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, viewMe(currentUser(r)))
}

func (s *Server) apiUpdateMe(w http.ResponseWriter, r *http.Request) {
	var b struct {
		DisplayName string `json:"display_name"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := strings.TrimSpace(b.DisplayName)
	if len([]rune(name)) > 64 {
		writeError(w, http.StatusBadRequest, "invalid", "display name is too long")
		return
	}
	u := currentUser(r)
	if err := s.store.SetDisplayName(r.Context(), u.ID, name); err != nil {
		s.internalError(w, r, err)
		return
	}
	u.DisplayName = name
	writeJSON(w, http.StatusOK, viewMe(u))
}

func (s *Server) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u := currentUser(r)
	keep := loginHash(r)
	if err := s.auth.ChangePassword(r.Context(), u, b.Current, b.Next, keep); err != nil {
		if errors.Is(err, auth.ErrInvalidLogin) {
			writeError(w, http.StatusUnprocessableEntity, "wrong_password", "current password is wrong")
			return
		}
		s.writeAccountError(w, r, err)
		return
	}
	// Other browsers are signed out; their terminal sockets go with them.
	s.sockMu.Lock()
	for ls := range s.socks[u.ID] {
		if ls.loginHash != keep {
			ls.cancel()
		}
	}
	s.sockMu.Unlock()
	s.audit(r, "password.change", u.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.UserSettings(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(data))
}

func (s *Server) apiPutSettings(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || len(raw) > 64<<10 {
		writeError(w, http.StatusBadRequest, "bad_request", "settings must be a JSON object under 64 KiB")
		return
	}
	if err := s.store.SaveUserSettings(r.Context(), currentUser(r).ID, string(raw)); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiInviteInfo(w http.ResponseWriter, r *http.Request) {
	inv, err := s.auth.InviteInfo(r.Context(), r.PathValue("token"))
	if err != nil {
		writeError(w, http.StatusNotFound, "bad_invite", "this invite link is invalid, used or expired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": inv.Role, "expires_at": inv.ExpiresAt})
}

func (s *Server) apiInviteRedeem(w http.ResponseWriter, r *http.Request) {
	if !s.auth.AllowAttempt(s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
		return
	}
	var b credentialsBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u, err := s.auth.RedeemInvite(r.Context(), r.PathValue("token"), strings.TrimSpace(b.Username), strings.TrimSpace(b.DisplayName), b.Password)
	if err != nil {
		s.writeAccountError(w, r, err)
		return
	}
	s.auditAs(r, u, "invite.redeem", u.Username, map[string]any{"role": u.Role})
	s.signIn(w, r, u)
}

func (s *Server) writeAccountError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrBadUsername):
		writeError(w, http.StatusUnprocessableEntity, "bad_username", err.Error())
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusUnprocessableEntity, "weak_password", err.Error())
	case errors.Is(err, store.ErrUsernameTaken):
		writeError(w, http.StatusConflict, "username_taken", "this username is already in use")
	case errors.Is(err, auth.ErrBadInvite):
		writeError(w, http.StatusNotFound, "bad_invite", err.Error())
	case errors.Is(err, auth.ErrBadSetupToken):
		writeError(w, http.StatusForbidden, "bad_setup_token", "the setup link is wrong; copy it again from the server log")
	case errors.Is(err, auth.ErrSetupDone):
		writeError(w, http.StatusConflict, "setup_done", "setup is already complete")
	case errors.Is(err, auth.ErrBusy):
		writeError(w, http.StatusServiceUnavailable, "busy", "the server is busy, try again in a moment")
	default:
		s.internalError(w, r, err)
	}
}

// Admin --------------------------------------------------------------------

type adminUserView struct {
	*store.User
	Sessions int `json:"open_sessions"`
}

func (s *Server) apiAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]adminUserView, 0, len(users))
	for _, u := range users {
		n := 0
		for _, info := range s.terms.List(u.ID) {
			if info.State == "running" {
				n++
			}
		}
		out = append(out, adminUserView{User: u, Sessions: n})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	var b struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	target, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	if b.Role != nil && *b.Role != store.RoleAdmin && *b.Role != store.RoleMember {
		writeError(w, http.StatusBadRequest, "invalid", "unknown role")
		return
	}
	if b.Role == nil && b.Disabled == nil {
		writeJSON(w, http.StatusOK, target)
		return
	}
	// One transaction: the change is refused if it would leave no active administrator.
	if err := s.store.UpdateUserGuarded(r.Context(), id, b.Role, b.Disabled); err != nil {
		if errors.Is(err, store.ErrLastAdmin) {
			writeError(w, http.StatusConflict, "last_admin", "at least one active administrator must remain")
			return
		}
		s.internalError(w, r, err)
		return
	}
	if b.Role != nil {
		s.audit(r, "user.role", target.Username, map[string]any{"role": *b.Role})
	}
	if b.Disabled != nil {
		if *b.Disabled {
			if err := s.auth.EndAccess(r.Context(), id); err != nil {
				s.internalError(w, r, err)
				return
			}
			s.audit(r, "user.disable", target.Username, nil)
		} else {
			s.audit(r, "user.enable", target.Username, nil)
		}
	}
	target, _ = s.store.UserByID(r.Context(), id)
	writeJSON(w, http.StatusOK, target)
}

func (s *Server) apiAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	var b struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	target, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	if err := s.auth.ResetPassword(r.Context(), id, b.Password); err != nil {
		s.writeAccountError(w, r, err)
		return
	}
	s.audit(r, "user.password_reset", target.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiAdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Role     string `json:"role"`
		Note     string `json:"note"`
		TTLHours int    `json:"ttl_hours"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if b.TTLHours <= 0 || b.TTLHours > 24*30 {
		b.TTLHours = 72
	}
	tok, exp, err := s.auth.CreateInvite(r.Context(), currentUser(r), b.Role, truncate(strings.TrimSpace(b.Note), 120), time.Duration(b.TTLHours)*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	s.audit(r, "invite.create", b.Role, map[string]any{"note": b.Note, "expires_at": exp})
	writeJSON(w, http.StatusOK, map[string]any{
		"url":        s.cfg.Origin() + "/invite/" + tok,
		"expires_at": exp,
	})
}

func (s *Server) apiAdminAudit(w http.ResponseWriter, r *http.Request) {
	q := store.AuditQuery{Limit: 100}
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := parseInt(v); err == nil {
			q.BeforeID = n
		}
	}
	if v := r.URL.Query().Get("user"); v != "" {
		if n, err := parseInt(v); err == nil {
			q.UserID = &n
		}
	}
	entries, err := s.store.ListAudit(r.Context(), q)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
