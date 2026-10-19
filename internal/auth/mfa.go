package auth

// Two-step sign-in. A password that is right for an account with an
// authenticator yields a short-lived sign-in challenge instead of a login;
// the challenge plus a one-time code (or a recovery code) yields the login.
// Wrong codes count as failed sign-ins (the account backs off as for
// passwords) and are limited per account wherever a code is checked.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/vault"
)

var (
	ErrBadCode       = errors.New("wrong, expired or already used code")
	ErrChallengeGone = errors.New("the sign-in step expired; sign in again")
	ErrNoEnrolment   = errors.New("no authenticator set-up in progress; start again")
	ErrTOTPOff       = errors.New("two-step sign-in is not on")
	ErrTOTPOn        = errors.New("two-step sign-in is already on")
	ErrChanged       = errors.New("the account changed meanwhile; sign in again")
)

const (
	challengeTTL   = 5 * time.Minute
	challengeTries = 5
	maxChallenges  = 10000
	enrolTTL       = 10 * time.Minute
	codeFailures   = 10               // wrong codes or passwords per account per codeWindow
	codeWindow     = 15 * time.Minute // before more are refused
)

// MFARequiredError is Login's answer for an account with two-step sign-in:
// Token goes back with the code.
type MFARequiredError struct{ Token string }

func (e *MFARequiredError) Error() string { return "a one-time code is required" }

type challenge struct {
	userID  int64
	pwHash  string // the password verified; the login is refused if it changed since
	expires time.Time
	tries   int
	busy    bool // a code for it is being checked; others wait their turn
}

type enrolment struct {
	secret  []byte
	pwHash  string // the password confirmed when it began
	expires time.Time
	tries   int
	busy    bool
}

type mfaState struct {
	mu         sync.Mutex
	challenges map[string]*challenge // by token hash
	enrol      map[int64]*enrolment
	fails      map[int64][]time.Time // recent wrong codes per account
}

// UseVault gives the service the vault that seals authenticator secrets.
func (s *Service) UseVault(v *vault.Vault) { s.vault = v }

func totpAAD(userID int64) []byte { return []byte("totp:" + strconv.FormatInt(userID, 10)) }

func (s *Service) mfa() *mfaState {
	s.mfaOnce.Do(func() {
		s.mfaS = &mfaState{challenges: map[string]*challenge{}, enrol: map[int64]*enrolment{}, fails: map[int64][]time.Time{}}
	})
	return s.mfaS
}

// newChallenge starts the code step of a sign-in.
func (s *Service) newChallenge(u *store.User) (string, error) {
	tok, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	m := s.mfa()
	now := s.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.challenges) >= maxChallenges {
		for h, c := range m.challenges {
			if now.After(c.expires) {
				delete(m.challenges, h)
			}
		}
		if len(m.challenges) >= maxChallenges {
			return "", ErrRateLimited
		}
	}
	m.challenges[hash] = &challenge{userID: u.ID, pwHash: u.PasswordHash(), expires: now.Add(challengeTTL)}
	return tok, nil
}

// takeAttempt reserves one of the account's tries before a code or password
// is checked, so concurrent requests cannot together exceed the limit;
// done(true) gives a successful try back.
func (s *Service) takeAttempt(userID int64) (done func(ok bool), allowed bool) {
	m := s.mfa()
	now := s.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	recent := m.fails[userID][:0]
	for _, t := range m.fails[userID] {
		if now.Sub(t) < codeWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= codeFailures {
		m.fails[userID] = recent
		return nil, false
	}
	m.fails[userID] = append(recent, now)
	return func(ok bool) {
		if !ok {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		l := m.fails[userID]
		for i := len(l) - 1; i >= 0; i-- {
			if l[i].Equal(now) {
				m.fails[userID] = append(l[:i], l[i+1:]...)
				break
			}
		}
		if len(m.fails[userID]) == 0 {
			delete(m.fails, userID)
		}
	}, true
}

// verifyCode checks a one-time or recovery code of an account with two-step
// sign-in on, consuming it, and returns the sealed secret it was checked
// against: a change made on its strength applies only to that set-up. Wrong
// codes are counted against the account; a check that failed for another
// reason (the server's) is not.
func (s *Service) verifyCode(ctx context.Context, u *store.User, code string) ([]byte, error) {
	done, allowed := s.takeAttempt(u.ID)
	if !allowed {
		return nil, ErrRateLimited
	}
	ct, ok, err := s.checkCode(ctx, u, code)
	done(ok || (err != nil && !errors.Is(err, ErrTOTPOff)))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrBadCode
	}
	return ct, nil
}

func (s *Service) checkCode(ctx context.Context, u *store.User, code string) ([]byte, bool, error) {
	t, err := s.store.UserTOTP(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, ErrTOTPOff
	}
	if err != nil {
		return nil, false, err
	}
	if looksLikeRecovery(code) {
		ok, err := s.store.UseRecoveryCode(ctx, u.ID, RecoveryHash(code))
		return t.Ciphertext, ok, err
	}
	if s.vault == nil {
		return nil, false, errors.New("no vault for authenticator secrets")
	}
	secret, err := s.vault.Open(vault.Sealed{Ciphertext: t.Ciphertext, WrappedKey: t.WrappedKey, KeyID: t.KeyID}, totpAAD(u.ID))
	if err != nil {
		return nil, false, fmt.Errorf("open authenticator secret: %w", err)
	}
	step, ok := CheckTOTP(secret, code, s.now(), t.LastStep)
	if !ok {
		return nil, false, nil
	}
	// Recorded only if no one used this (or a later) step meanwhile.
	ok, err = s.store.UseTOTPStep(ctx, u.ID, t.Ciphertext, step)
	return t.Ciphertext, ok, err
}

// LoginMFA completes a sign-in that Login answered with MFARequiredError.
// The user is returned with an error too when known, for the audit log.
func (s *Service) LoginMFA(ctx context.Context, token, code, ip, ua string) (*store.User, string, error) {
	if !s.AllowAttempt(ip) {
		return nil, "", ErrRateLimited
	}
	m := s.mfa()
	hash := HashToken(token)
	now := s.now()
	m.mu.Lock()
	c := m.challenges[hash]
	if c != nil && (now.After(c.expires) || c.tries >= challengeTries) {
		delete(m.challenges, hash)
		c = nil
	}
	busy := c != nil && c.busy
	if c != nil && !busy {
		c.tries++
		c.busy = true
	}
	m.mu.Unlock()
	if c == nil {
		return nil, "", ErrChallengeGone
	}
	if busy {
		return nil, "", ErrRateLimited // one code at a time per sign-in
	}
	// release ends this request's turn; drop also ends the challenge.
	release := func(drop bool) {
		m.mu.Lock()
		if drop {
			delete(m.challenges, hash)
		} else {
			c.busy = false
		}
		m.mu.Unlock()
	}
	u, err := s.store.UserByID(ctx, c.userID)
	if err != nil {
		release(true)
		return nil, "", ErrChallengeGone
	}
	if until := time.Unix(u.LockedUntil(), 0); until.After(now) {
		release(false)
		return u, "", &LockedError{RetryAfter: until.Sub(now).Round(time.Second)}
	}
	if u.Disabled || u.PasswordHash() != c.pwHash {
		release(true)
		return nil, "", ErrChallengeGone
	}
	if _, err := s.verifyCode(ctx, u, code); err != nil {
		if errors.Is(err, ErrTOTPOff) {
			release(true)
			return nil, "", ErrChallengeGone
		}
		release(false)
		if errors.Is(err, ErrBadCode) {
			if ferr := s.recordFailure(ctx, u, now); ferr != nil {
				return u, "", ferr
			}
		}
		return u, "", err
	}
	release(true)
	if err := s.store.ResetLoginFailures(ctx, u.ID); err != nil {
		return nil, "", err
	}
	tok, thash, err := NewToken()
	if err != nil {
		return nil, "", err
	}
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.store.CreateLoginSessionIfCurrent(ctx, thash, u.ID, c.pwHash, ip, ua, false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", ErrChallengeGone
		}
		return nil, "", err
	}
	return u, tok, nil
}

// Enrolment ---------------------------------------------------------------

// BeginTOTP starts setting up an authenticator, after the password is
// confirmed. The secret stays in memory until a code from it is entered.
func (s *Service) BeginTOTP(ctx context.Context, u *store.User, password string) ([]byte, error) {
	if err := s.confirmPassword(u, password); err != nil {
		return nil, err
	}
	if u.TOTP {
		return nil, ErrTOTPOn
	}
	secret := NewTOTPSecret()
	m := s.mfa()
	m.mu.Lock()
	m.enrol[u.ID] = &enrolment{secret: secret, pwHash: u.PasswordHash(), expires: s.now().Add(enrolTTL)}
	m.mu.Unlock()
	return secret, nil
}

// EnableTOTP finishes the set-up with a code from the new authenticator and
// returns the recovery codes, shown once. sessionHash is the login asking.
func (s *Service) EnableTOTP(ctx context.Context, u *store.User, code, sessionHash string) ([]string, error) {
	if s.vault == nil {
		return nil, errors.New("no vault for authenticator secrets")
	}
	m := s.mfa()
	now := s.now()
	m.mu.Lock()
	e := m.enrol[u.ID]
	if e != nil && (now.After(e.expires) || e.tries >= challengeTries) {
		delete(m.enrol, u.ID)
		e = nil
	}
	busy := e != nil && e.busy
	if e != nil && !busy {
		e.tries++
		e.busy = true
	}
	m.mu.Unlock()
	if e == nil {
		return nil, ErrNoEnrolment
	}
	if busy {
		return nil, ErrRateLimited
	}
	finish := func(done bool) {
		m.mu.Lock()
		if done && m.enrol[u.ID] == e {
			delete(m.enrol, u.ID)
		}
		e.busy = false
		m.mu.Unlock()
	}
	step, ok := CheckTOTP(e.secret, code, now, 0)
	if !ok {
		finish(false)
		return nil, ErrBadCode
	}
	sealed, err := s.vault.Seal(e.secret, totpAAD(u.ID))
	if err != nil {
		finish(false)
		return nil, err
	}
	codes, hashes := NewRecoveryCodes()
	err = s.store.EnableTOTP(ctx, u.ID, store.TOTPSecret{Ciphertext: sealed.Ciphertext, WrappedKey: sealed.WrappedKey, KeyID: sealed.KeyID, LastStep: step},
		hashes, e.pwHash, sessionHash)
	finish(err == nil || errors.Is(err, store.ErrNotFound))
	if errors.Is(err, store.ErrNotFound) {
		// Turned on meanwhile (another set-up), or the password or login changed.
		return nil, ErrChanged
	}
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableTOTP turns two-step sign-in off; it takes the password and a code.
func (s *Service) DisableTOTP(ctx context.Context, u *store.User, password, code, sessionHash string) error {
	if err := s.confirmPassword(u, password); err != nil {
		return err
	}
	if !u.TOTP {
		return ErrTOTPOff
	}
	ct, err := s.verifyCode(ctx, u, code)
	if err != nil {
		return err
	}
	if err := s.store.DisableTOTP(ctx, u.ID, store.OwnChange{PasswordHash: u.PasswordHash(), SessionHash: sessionHash, Ciphertext: ct}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrChanged
		}
		return err
	}
	return nil
}

// NewRecovery replaces the recovery codes; it takes the password and a code.
func (s *Service) NewRecovery(ctx context.Context, u *store.User, password, code, sessionHash string) ([]string, error) {
	if err := s.confirmPassword(u, password); err != nil {
		return nil, err
	}
	if !u.TOTP {
		return nil, ErrTOTPOff
	}
	ct, err := s.verifyCode(ctx, u, code)
	if err != nil {
		return nil, err
	}
	codes, hashes := NewRecoveryCodes()
	if err := s.store.ReplaceRecoveryCodes(ctx, u.ID, hashes, store.OwnChange{PasswordHash: u.PasswordHash(), SessionHash: sessionHash, Ciphertext: ct}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrChanged
		}
		return nil, err
	}
	return codes, nil
}

// ResetTOTP is the admin path for a lost authenticator.
func (s *Service) ResetTOTP(ctx context.Context, userID int64) error {
	m := s.mfa()
	m.mu.Lock()
	delete(m.enrol, userID)
	delete(m.fails, userID)
	m.mu.Unlock()
	return s.store.DisableTOTP(ctx, userID, store.OwnChange{})
}

// confirmPassword re-checks the password of a signed-in user before a
// change to how they sign in; wrong ones count like wrong codes.
func (s *Service) confirmPassword(u *store.User, password string) error {
	done, allowed := s.takeAttempt(u.ID)
	if !allowed {
		return ErrRateLimited
	}
	ok, err := CheckPassword(password, u.PasswordHash())
	done(ok || err != nil) // a busy server is not a wrong password
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidLogin
	}
	return nil
}
