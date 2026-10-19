package web

// Two-step sign-in: the code step of signing in, the user's own set-up, and
// the admin reset for a lost authenticator.

import (
	"errors"
	"net/http"

	"github.com/ihainan/corral/internal/auth"
)

// writeMFAError answers the errors of code checks and set-up.
func (s *Server) writeMFAError(w http.ResponseWriter, r *http.Request, err error) {
	var locked *auth.LockedError
	switch {
	case errors.As(err, &locked):
		writeErrorExtra(w, http.StatusTooManyRequests, "locked", "too many failed attempts",
			map[string]any{"retry_after": int(locked.RetryAfter.Seconds())})
	case errors.Is(err, auth.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
	case errors.Is(err, auth.ErrBusy):
		writeError(w, http.StatusServiceUnavailable, "busy", "the server is busy, try again in a moment")
	case errors.Is(err, auth.ErrBadCode):
		writeError(w, http.StatusUnprocessableEntity, "bad_code", "wrong, expired or already used code")
	case errors.Is(err, auth.ErrChallengeGone):
		writeError(w, http.StatusUnauthorized, "mfa_expired", "the sign-in step expired; sign in again")
	case errors.Is(err, auth.ErrNoEnrolment):
		writeError(w, http.StatusConflict, "no_enrolment", "start the set-up again")
	case errors.Is(err, auth.ErrInvalidLogin):
		writeError(w, http.StatusUnprocessableEntity, "wrong_password", "the password is wrong")
	case errors.Is(err, auth.ErrTOTPOff):
		writeError(w, http.StatusConflict, "totp_off", "two-step sign-in is not on")
	case errors.Is(err, auth.ErrChanged):
		writeError(w, http.StatusConflict, "account_changed", "the account changed meanwhile; reload and try again")
	case errors.Is(err, auth.ErrTOTPOn):
		writeError(w, http.StatusConflict, "totp_on", "two-step sign-in is already on")
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) apiLoginMFA(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}
	if err := readJSON(r, &b); err != nil || b.Token == "" || len(b.Code) > 32 {
		writeError(w, http.StatusBadRequest, "bad_request", "token and code are required")
		return
	}
	u, tok, err := s.auth.LoginMFA(r.Context(), b.Token, b.Code, s.clientIP(r), r.UserAgent())
	if err != nil {
		// Recorded against the account (not as its action: it is not signed in).
		var locked *auth.LockedError
		switch {
		case u != nil && errors.Is(err, auth.ErrBadCode):
			s.auditAs(r, nil, "login.code_failed", u.Username, nil)
		case u != nil && (errors.As(err, &locked) || errors.Is(err, auth.ErrRateLimited)):
			s.auditAs(r, nil, "login.locked", u.Username, map[string]any{"two_step": true})
		}
		s.writeMFAError(w, r, err)
		return
	}
	s.auditAs(r, u, "login.success", u.Username, map[string]any{"two_step": true})
	s.setSessionCookie(w, tok)
	writeJSON(w, http.StatusOK, map[string]any{"user": viewMe(u)})
}

func (s *Server) apiTOTPStatus(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	n, err := s.store.CountRecoveryCodes(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"on": u.TOTP, "recovery_left": n})
}

func (s *Server) apiTOTPBegin(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u := currentUser(r)
	secret, err := s.auth.BeginTOTP(r.Context(), u, b.Password)
	if err != nil {
		s.writeMFAError(w, r, err)
		return
	}
	account := u.Username + "@" + s.cfg.PublicURL.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": auth.TOTPSecretText(secret),
		"uri":    auth.TOTPURI(secret, "Corral", account),
	})
}

func (s *Server) apiTOTPEnable(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u := currentUser(r)
	codes, err := s.auth.EnableTOTP(r.Context(), u, b.Code, loginHash(r))
	if err != nil {
		s.writeMFAError(w, r, err)
		return
	}
	s.audit(r, "totp.enable", u.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type passwordAndCode struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) apiTOTPDisable(w http.ResponseWriter, r *http.Request) {
	var b passwordAndCode
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u := currentUser(r)
	if err := s.auth.DisableTOTP(r.Context(), u, b.Password, b.Code, loginHash(r)); err != nil {
		s.writeMFAError(w, r, err)
		return
	}
	s.audit(r, "totp.disable", u.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiTOTPRecovery(w http.ResponseWriter, r *http.Request) {
	var b passwordAndCode
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u := currentUser(r)
	codes, err := s.auth.NewRecovery(r.Context(), u, b.Password, b.Code, loginHash(r))
	if err != nil {
		s.writeMFAError(w, r, err)
		return
	}
	s.audit(r, "totp.recovery_codes", u.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// apiAdminResetTOTP turns a user's two-step sign-in off, for a lost phone.
// Their logins stay: whoever lost the phone is usually still signed in, and
// the reset only lowers what a sign-in requires.
func (s *Server) apiAdminResetTOTP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	target, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	if err := s.auth.ResetTOTP(r.Context(), id); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "admin.totp_reset", target.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
