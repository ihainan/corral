// Package auth handles accounts: password hashing, login throttling, browser
// sessions, first-run setup and invites.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP's 64 MiB / t=2 tier). Hashing is gated by a small
// semaphore so a burst of logins cannot exhaust memory.
const (
	argonMemory  = 64 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

var hashSlots = make(chan struct{}, 4)

// ErrBusy means every hashing slot stayed taken for too long (a flood of
// sign-in attempts); the caller should answer "try again later".
var ErrBusy = errors.New("server is busy, try again")

const (
	slotWait   = 5 * time.Second
	maxWaiters = 64 // beyond this, new requests are refused at once instead of queueing
)

var waiters atomic.Int32

func acquireSlot() bool {
	select {
	case hashSlots <- struct{}{}:
		return true
	default:
	}
	if waiters.Add(1) > maxWaiters {
		waiters.Add(-1)
		return false
	}
	defer waiters.Add(-1)
	t := time.NewTimer(slotWait)
	defer t.Stop()
	select {
	case hashSlots <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

const (
	MinPasswordLen = 10
	MaxPasswordLen = 256
)

var ErrWeakPassword = fmt.Errorf("password must be %d–%d characters", MinPasswordLen, MaxPasswordLen)

func CheckPasswordPolicy(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen || len(pw) > MaxPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if !acquireSlot() {
		return "", ErrBusy
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-hashSlots
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches; a busy server counts as a mismatch.
// Login paths that track failures use CheckPassword to tell the two apart.
func VerifyPassword(pw, encoded string) bool {
	ok, _ := CheckPassword(pw, encoded)
	return ok
}

// CheckPassword returns ErrBusy when no hashing slot became free in time, so a
// flood of requests cannot make a correct password look wrong (and lock the account).
func CheckPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, nil
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, nil
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, nil
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, nil
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, nil
	}
	// Refuse absurd parameters from a tampered hash instead of burning memory.
	if m > 256*1024 || t > 10 || p > 8 || len(want) > 64 {
		return false, nil
	}
	if !acquireSlot() {
		return false, ErrBusy
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-hashSlots
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when the username does not exist, so a miss
// costs the same time as a wrong password.
var dummyHash = func() string {
	h, err := HashPassword("corral-dummy-password-for-timing")
	if err != nil {
		panic(err)
	}
	return h
}()

// NewToken returns a random bearer token and the hash that gets stored.
// Only hashes reach the database, so a leaked backup cannot be replayed as logins.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
