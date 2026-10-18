package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	// Not tied to the request: a browser giving up must not leave the login
	// (and the push subscriptions and forward access hanging off it) behind.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	err := s.auth.Logout(ctx, hash)
	s.closeSockets(currentUser(r).ID, hash) // what is open ends either way
	if err != nil {
		s.log.Error("sign-out failed", "err", err)
		writeError(w, http.StatusInternalServerError, "logout_failed", "could not sign out; try again")
		return
	}
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
	// Other browsers are signed out; their sockets and pending connects go with them.
	s.closeOtherLogins(u.ID, keep)
	s.audit(r, "password.change", u.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Settings are one JSON object of fields plus "_w": for each field the
// writer (one page load of the app) and that writer's request number that
// last set it. A later arrival wins, which is all settings need, except that
// a writer's own older request never undoes its newer one (a keepalive sent
// while the page closed may overtake a normal save). No client clock is
// involved. Every request also names the account it was made for, so a tab
// that missed a sign-in elsewhere cannot write into the new account.

const settingsWriters = "_w"

// settingRules lists the known settings and checks their values.
var settingRules = map[string]func(json.RawMessage) bool{
	"theme":        oneOf("system", "light", "dark"),
	"termTheme":    matches(regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)),
	"fontSize":     intRange(9, 28),
	"keybar":       isBool,
	"optionIsMeta": isBool,
	"webgl":        isBool,
	"webhook":      webhookURL,
}

func oneOf(vals ...string) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var v string
		return json.Unmarshal(raw, &v) == nil && slices.Contains(vals, v)
	}
}

func matches(re *regexp.Regexp) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var v string
		return json.Unmarshal(raw, &v) == nil && re.MatchString(v)
	}
}

func intRange(lo, hi int) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var v int
		return json.Unmarshal(raw, &v) == nil && v >= lo && v <= hi && strings.TrimSpace(string(raw)) == strconv.Itoa(v)
	}
}

// webhookURL is empty (off) or an https URL without spaces; whether its
// prefix is allowed is checked when sending.
func webhookURL(raw json.RawMessage) bool {
	var v string
	if json.Unmarshal(raw, &v) != nil || len(v) > 2048 {
		return false
	}
	return v == "" || (strings.HasPrefix(v, "https://") && len(v) > len("https://") && !strings.ContainsAny(v, " \t\r\n"))
}

func isBool(raw json.RawMessage) bool {
	var v bool
	return json.Unmarshal(raw, &v) == nil
}

type settingWriter struct {
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
}

var writerIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.UserSettings(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &obj) != nil || obj == nil {
		obj = map[string]json.RawMessage{}
	}
	out := map[string]json.RawMessage{}
	for k, v := range obj {
		if rule, ok := settingRules[k]; ok && rule(v) { // stored before validation existed: skip what no longer passes
			out[k] = v
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiPatchSettings(w http.ResponseWriter, r *http.Request) {
	var b struct {
		User   int64                      `json:"user"`
		Writer string                     `json:"writer"`
		Seq    int64                      `json:"seq"`
		Set    map[string]json.RawMessage `json:"set"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if b.User != currentUser(r).ID {
		writeError(w, http.StatusConflict, "user_mismatch", "these settings were made for another account")
		return
	}
	if !writerIDRE.MatchString(b.Writer) || b.Seq <= 0 || len(b.Set) == 0 || len(b.Set) > len(settingRules) {
		writeError(w, http.StatusBadRequest, "bad_request", "send a writer id, a positive seq and the changed settings")
		return
	}
	for k, v := range b.Set {
		if rule, ok := settingRules[k]; !ok || !rule(v) {
			writeError(w, http.StatusBadRequest, "bad_setting", "unknown setting or invalid value: "+k)
			return
		}
	}
	var accepted, superseded []string
	err := s.store.MergeUserSettings(r.Context(), currentUser(r).ID, func(old string) (string, error) {
		accepted, superseded = nil, nil
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(old), &obj) != nil || obj == nil {
			obj = map[string]json.RawMessage{}
		}
		writers := map[string]settingWriter{}
		if raw, ok := obj[settingsWriters]; ok {
			_ = json.Unmarshal(raw, &writers)
		}
		delete(obj, "_ts") // bookkeeping of an earlier format
		for k, v := range b.Set {
			if prev, ok := writers[k]; ok && prev.ID == b.Writer && prev.Seq >= b.Seq {
				superseded = append(superseded, k) // this writer already sent something newer
				continue
			}
			obj[k], writers[k] = v, settingWriter{ID: b.Writer, Seq: b.Seq}
			accepted = append(accepted, k)
		}
		rawW, _ := json.Marshal(writers)
		obj[settingsWriters] = rawW
		out, err := json.Marshal(obj)
		if err != nil {
			return "", err
		}
		if len(out) > 64<<10 {
			return "", errSettingsTooLarge
		}
		return string(out), nil
	})
	if errors.Is(err, errSettingsTooLarge) {
		writeError(w, http.StatusBadRequest, "bad_request", "settings must stay under 64 KiB")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	sort.Strings(accepted)
	sort.Strings(superseded)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": accepted, "superseded": superseded})
}

var errSettingsTooLarge = errors.New("settings too large")

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
			// Logins were removed in the same transaction; now close what is open.
			s.auth.CloseConnections(id)
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
