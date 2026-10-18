package auth

import (
	"context"
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
	if HashToken(token) != s.setupHash {
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

// Login checks credentials and, on success, returns a new session token.
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (*store.User, string, error) {
	if !s.limiter.allow(ip, s.now()) {
		return nil, "", ErrRateLimited
	}
	u, err := s.store.UserByName(ctx, strings.TrimSpace(username))
	if errors.Is(err, store.ErrNotFound) {
		VerifyPassword(password, dummyHash)
		return nil, "", ErrInvalidLogin
	}
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	if until := time.Unix(u.LockedUntil(), 0); until.After(now) {
		return nil, "", &LockedError{RetryAfter: until.Sub(now).Round(time.Second)}
	}
	ok := VerifyPassword(password, u.PasswordHash())
	if !ok || u.Disabled {
		if !ok {
			failures := u.FailedLogins() + 1
			var lockUntil int64
			if failures >= freeAttempts {
				wait := time.Minute << min(failures-freeAttempts, 4)
				lockUntil = now.Add(min(wait, maxLockout)).Unix()
			}
			if err := s.store.RecordLoginFailure(ctx, u.ID, lockUntil); err != nil {
				return nil, "", err
			}
		}
		return nil, "", ErrInvalidLogin
	}
	if err := s.store.ResetLoginFailures(ctx, u.ID); err != nil {
		return nil, "", err
	}
	tok, err := s.startSession(ctx, u.ID, ip, ua)
	if err != nil {
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
	ls, err := s.store.LoginSessionByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", ErrUnauthorized
	}
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	if now.Sub(time.Unix(ls.LastSeenAt, 0)) > s.idle || now.Sub(time.Unix(ls.CreatedAt, 0)) > s.maxAge {
		_ = s.store.DeleteLoginSession(ctx, hash)
		return nil, "", ErrUnauthorized
	}
	u, err := s.store.UserByID(ctx, ls.UserID)
	if err != nil {
		return nil, "", ErrUnauthorized
	}
	if u.Disabled {
		_ = s.store.DeleteLoginSession(ctx, hash)
		return nil, "", ErrUnauthorized
	}
	s.touch(ctx, hash, now)
	return u, hash, nil
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
// out every other browser.
func (s *Service) ChangePassword(ctx context.Context, u *store.User, current, next, keepSession string) error {
	if !VerifyPassword(current, u.PasswordHash()) {
		return ErrInvalidLogin
	}
	if err := CheckPasswordPolicy(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.store.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	return s.store.DeleteUserLoginSessions(ctx, u.ID, keepSession)
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
	if err := s.store.SetPassword(ctx, userID, hash); err != nil {
		return err
	}
	return s.EndAccess(ctx, userID)
}

// EndAccess signs a user out everywhere and closes everything they had open.
func (s *Service) EndAccess(ctx context.Context, userID int64) error {
	if err := s.store.DeleteUserLoginSessions(ctx, userID, ""); err != nil {
		return err
	}
	if s.OnUserGone != nil {
		s.OnUserGone(userID)
	}
	return nil
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
