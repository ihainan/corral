package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/store"
)

var (
	ErrInvalidLogin  = errors.New("invalid username or password")
	ErrLocked        = errors.New("too many failed attempts")
	ErrRateLimited   = errors.New("too many attempts from this address")
	ErrSetupDone     = errors.New("setup already completed")
	ErrBadSetupToken = errors.New("invalid setup token")
	ErrBadInvite     = errors.New("invite is invalid, used or expired")
	ErrBadUsername   = errors.New("username must be 2–32 characters: letters, digits, dot, dash or underscore")
	ErrUnauthorized  = errors.New("not signed in")
)

// Failed logins before the account starts backing off, and the cap on the wait.
// Backoff is per account (not per address) so one attacker cannot lock users out
// from everywhere for long, while password guessing still slows to a crawl.
const (
	freeAttempts = 5
	maxLockout   = 15 * time.Minute
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,31}$`)

func CheckUsername(u string) error {
	if !usernameRE.MatchString(u) {
		return ErrBadUsername
	}
	return nil
}

type Service struct {
	store       *store.Store
	idle        time.Duration
	maxAge      time.Duration
	now         func() time.Time
	limiter     *ipLimiter
	setupMu     sync.Mutex
	setupHash   string
	OnUserGone  func(userID int64) // called when a user's access must end now (disable, password change)
	touchMu     sync.Mutex
	lastTouched map[string]time.Time
}

type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string { return ErrLocked.Error() }
func (e *LockedError) Unwrap() error { return ErrLocked }

func NewService(st *store.Store, idle, maxAge time.Duration) *Service {
	return &Service{
		store:       st,
		idle:        idle,
		maxAge:      maxAge,
		now:         time.Now,
		limiter:     newIPLimiter(30, 5*time.Minute),
		lastTouched: map[string]time.Time{},
	}
}

// SetupNeeded reports whether no account exists yet.
func (s *Service) SetupNeeded(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	return n == 0, err
}

// PrepareSetup issues a one-time token for creating the first admin. It lives
// only in memory and is printed to the server log, so only someone with access
// to the server can claim a fresh install.
func (s *Service) PrepareSetup(ctx context.Context) (string, error) {
	needed, err := s.SetupNeeded(ctx)
	if err != nil || !needed {
		return "", err
	}
	tok, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	s.setupMu.Lock()
	s.setupHash = hash
	s.setupMu.Unlock()
	return tok, nil
}

func (s *Service) CompleteSetup(ctx context.Context, token, username, displayName, password string) (*store.User, error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupHash == "" {
		return nil, ErrSetupDone
	}
	if subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(s.setupHash)) != 1 {
		return nil, ErrBadSetupToken
	}
	if err := CheckUsername(username); err != nil {
		return nil, err
	}
	if err := CheckPasswordPolicy(password); err != nil {
		return nil, err
	}
	needed, err := s.SetupNeeded(ctx)
	if err != nil {
		return nil, err
	}
	if !needed {
		s.setupHash = ""
		return nil, ErrSetupDone
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u, err := s.store.CreateUser(ctx, username, displayName, hash, store.RoleAdmin)
	if err != nil {
		return nil, err
	}
	s.setupHash = ""
	return u, nil
}

// AllowAttempt applies the per-address limit shared by sign-in, setup and
// invite redemption (everything that hashes a password for an anonymous caller).
func (s *Service) AllowAttempt(ip string) bool { return s.limiter.allow(ip, s.now()) }

// Login checks credentials and, on success, returns a new session token.
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (*store.User, string, error) {
	if !s.AllowAttempt(ip) {
		return nil, "", ErrRateLimited
	}
	u, err := s.store.UserByName(ctx, strings.TrimSpace(username))
	if errors.Is(err, store.ErrNotFound) {
		if _, err := CheckPassword(password, dummyHash); errors.Is(err, ErrBusy) {
			return nil, "", ErrBusy
		}
		return nil, "", ErrInvalidLogin
	}
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	if until := time.Unix(u.LockedUntil(), 0); until.After(now) {
		return nil, "", &LockedError{RetryAfter: until.Sub(now).Round(time.Second)}
	}
	ok, err := CheckPassword(password, u.PasswordHash())
	if err != nil {
		return nil, "", err // busy: not the user's fault, not counted
	}
	if !ok {
		failures, err := s.store.RecordLoginFailure(ctx, u.ID)
		if err != nil {
			return nil, "", err
		}
		if failures >= freeAttempts {
			wait := min(time.Minute<<min(failures-freeAttempts, 4), maxLockout)
			if err := s.store.ExtendLockout(ctx, u.ID, now.Add(wait).Unix()); err != nil {
				return nil, "", err
			}
		}
		return nil, "", ErrInvalidLogin
	}
	if u.Disabled {
		return nil, "", ErrInvalidLogin
	}
	if err := s.store.ResetLoginFailures(ctx, u.ID); err != nil {
		return nil, "", err
	}
	tok, hash, err := NewToken()
	if err != nil {
		return nil, "", err
	}
	if len(ua) > 300 {
		ua = ua[:300]
	}
	// Created only if the password we just verified is still the current one.
	if err := s.store.CreateLoginSessionIfCurrent(ctx, hash, u.ID, u.PasswordHash(), ip, ua); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", ErrInvalidLogin
		}
		return nil, "", err
	}
	return u, tok, nil
}

func (s *Service) startSession(ctx context.Context, userID int64, ip, ua string) (string, error) {
	tok, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.store.CreateLoginSession(ctx, hash, userID, ip, ua); err != nil {
		return "", err
	}
	return tok, nil
}

// StartSession creates a login for a user who just proved who they are some
// other way (setup, invite redemption).
func (s *Service) StartSession(ctx context.Context, userID int64, ip, ua string) (string, error) {
	return s.startSession(ctx, userID, ip, ua)
}

// Authenticate resolves a session token to its user, enforcing expiry and
// account state on every call.
func (s *Service) Authenticate(ctx context.Context, token string) (*store.User, string, error) {
	if token == "" {
		return nil, "", ErrUnauthorized
	}
	hash := HashToken(token)
	u, err := s.CheckLogin(ctx, hash)
	if err != nil {
		return nil, "", err
	}
	return u, hash, nil
}

// CheckLogin re-validates a login by its stored hash. Long-lived connections
// call it periodically so expiry, sign-out and disabling reach them too.
func (s *Service) CheckLogin(ctx context.Context, hash string) (*store.User, error) {
	u, err := s.validLogin(ctx, hash)
	if err != nil {
		return nil, err
	}
	s.touch(ctx, hash, s.now())
	return u, nil
}

// LoginValid reports whether a login is still in force, without counting the
// check as activity (used before sending to a device bound to that login).
func (s *Service) LoginValid(ctx context.Context, hash string) bool {
	_, err := s.validLogin(ctx, hash)
	return err == nil
}

func (s *Service) validLogin(ctx context.Context, hash string) (*store.User, error) {
	ls, err := s.store.LoginSessionByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.Sub(time.Unix(ls.LastSeenAt, 0)) > s.idle || now.Sub(time.Unix(ls.CreatedAt, 0)) > s.maxAge {
		_ = s.store.DeleteLoginSession(ctx, hash)
		return nil, ErrUnauthorized
	}
	u, err := s.store.UserByID(ctx, ls.UserID)
	if err != nil {
		return nil, ErrUnauthorized
	}
	// A login older than the current password is void, even if deleting it
	// after a password change or reset did not happen.
	if u.Disabled || ls.CreatedAt < u.PasswordChangedAt {
		_ = s.store.DeleteLoginSession(ctx, hash)
		return nil, ErrUnauthorized
	}
	return u, nil
}

// LoginExpiry is when a login ends at the latest, regardless of activity.
func (s *Service) LoginExpiry(ctx context.Context, hash string) (time.Time, error) {
	ls, err := s.store.LoginSessionByHash(ctx, hash)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(ls.CreatedAt, 0).Add(s.maxAge), nil
}

// touch refreshes last_seen at most once a minute per session to spare writes.
func (s *Service) touch(ctx context.Context, hash string, now time.Time) {
	s.touchMu.Lock()
	last := s.lastTouched[hash]
	if now.Sub(last) < time.Minute {
		s.touchMu.Unlock()
		return
	}
	s.lastTouched[hash] = now
	if len(s.lastTouched) > 10000 {
		s.lastTouched = map[string]time.Time{hash: now}
	}
	s.touchMu.Unlock()
	_ = s.store.TouchLoginSession(ctx, hash)
}

func (s *Service) Logout(ctx context.Context, sessionHash string) error {
	return s.store.DeleteLoginSession(ctx, sessionHash)
}

// ChangePassword verifies the current password, stores the new one and signs
// out every other browser, atomically with respect to admin resets.
func (s *Service) ChangePassword(ctx context.Context, u *store.User, current, next, keepSession string) error {
	ok, err := CheckPassword(current, u.PasswordHash())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidLogin
	}
	if err := CheckPasswordPolicy(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.store.ChangePasswordIf(ctx, u.ID, u.PasswordHash(), hash, keepSession); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidLogin // password or login changed underneath us
		}
		return err
	}
	return nil
}

// ResetPassword is the admin path: new password, all sessions and terminals ended.
func (s *Service) ResetPassword(ctx context.Context, userID int64, next string) error {
	if err := CheckPasswordPolicy(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.store.ResetPasswordTx(ctx, userID, hash); err != nil {
		return err
	}
	s.CloseConnections(userID)
	return nil
}

// EndAccess signs a user out everywhere and closes everything they had open.
// It keeps going if the caller's request is cancelled: half-revoked access is
// worse than a slow response.
func (s *Service) EndAccess(ctx context.Context, userID int64) error {
	err := s.store.DeleteUserLoginSessions(context.WithoutCancel(ctx), userID, "")
	s.CloseConnections(userID)
	return err
}

// CloseConnections ends a user's terminals, sockets and pending connects. The
// logins themselves must already be gone (or the account disabled).
func (s *Service) CloseConnections(userID int64) {
	if s.OnUserGone != nil {
		s.OnUserGone(userID)
	}
}

// CreateInvite returns the raw invite token; only its hash is stored.
func (s *Service) CreateInvite(ctx context.Context, by *store.User, role, note string, ttl time.Duration) (string, int64, error) {
	if role != store.RoleAdmin && role != store.RoleMember {
		return "", 0, errors.New("invalid role")
	}
	tok, hash, err := NewToken()
	if err != nil {
		return "", 0, err
	}
	exp := s.now().Add(ttl).Unix()
	if err := s.store.CreateInvite(ctx, hash, role, note, by.ID, exp); err != nil {
		return "", 0, err
	}
	return tok, exp, nil
}

func (s *Service) InviteInfo(ctx context.Context, token string) (*store.Invite, error) {
	inv, err := s.store.InviteByHash(ctx, HashToken(token))
	if err != nil || inv.UsedAt != 0 || inv.ExpiresAt <= s.now().Unix() {
		return nil, ErrBadInvite
	}
	return inv, nil
}

func (s *Service) RedeemInvite(ctx context.Context, token, username, displayName, password string) (*store.User, error) {
	// Check the invite before spending a password hash on the request.
	if _, err := s.InviteInfo(ctx, token); err != nil {
		return nil, err
	}
	if err := CheckUsername(username); err != nil {
		return nil, err
	}
	if err := CheckPasswordPolicy(password); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u, err := s.store.RedeemInvite(ctx, HashToken(token), username, displayName, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrBadInvite
	}
	return u, err
}

// ipLimiter is a fixed-window counter per client address for login attempts.
type ipLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newIPLimiter(limit int, w time.Duration) *ipLimiter {
	return &ipLimiter{limit: limit, window: w, hits: map[string]*window{}}
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 50000 {
		for k, w := range l.hits {
			if now.Sub(w.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[ip]
	if w == nil || now.Sub(w.start) > l.window {
		l.hits[ip] = &window{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= l.limit
}
