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
	hashSlots <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-hashSlots
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false
	}
	hashSlots <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-hashSlots
	return subtle.ConstantTimeCompare(got, want) == 1
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

var errNoToken = errors.New("no token")
