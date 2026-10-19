package auth

// Time-based one-time codes (RFC 6238, as authenticator apps implement it:
// HMAC-SHA1, 6 digits, 30-second steps) and recovery codes.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1 // steps accepted either side of now, for clocks that drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret.
func NewTOTPSecret() []byte {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// TOTPSecretText is the secret as authenticator apps take it when typed in.
func TOTPSecretText(secret []byte) string { return b32.EncodeToString(secret) }

// TOTPURI is what the enrolment QR code holds.
func TOTPURI(secret []byte, issuer, account string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", TOTPSecretText(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func totpAt(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// CheckTOTP finds the step a code belongs to, near t and after lastStep (a
// code is good once). ok is false for a wrong, stale or reused code.
func CheckTOTP(secret []byte, code string, t time.Time, lastStep int64) (step int64, ok bool) {
	code = strings.Join(strings.Fields(code), "") // "123 456" as apps show it
	if len(code) != totpDigits {
		return 0, false
	}
	now := t.Unix() / totpPeriod
	found := int64(0)
	for s := now - totpSkew; s <= now+totpSkew; s++ {
		// Every candidate is compared, so timing does not tell which matched.
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, s)), []byte(code)) == 1 && s > lastStep && found == 0 {
			found = s
		}
	}
	return found, found != 0
}

// Recovery codes: ten of them, each good once, for a lost phone. Only their
// hashes are kept.

const recoveryCount = 10

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz023456789" // 32 symbols; no i, l, o or 1

// NewRecoveryCodes returns codes to show once, and the hashes to store.
func NewRecoveryCodes() (codes, hashes []string) {
	for range recoveryCount {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		var sb strings.Builder
		for i, c := range b {
			if i == 5 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[c&31]) // 32 symbols: no bias
		}
		codes = append(codes, sb.String())
		hashes = append(hashes, RecoveryHash(sb.String()))
	}
	return codes, hashes
}

// normRecovery ignores case, spaces and dashes, as people type codes.
func normRecovery(code string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return r
	}, code)
}

func RecoveryHash(code string) string {
	sum := sha256.Sum256([]byte("corral-recovery:" + normRecovery(code)))
	return hex.EncodeToString(sum[:])
}

// looksLikeRecovery tells a recovery code from a one-time code.
func looksLikeRecovery(code string) bool { return len(normRecovery(code)) == 10 }

// TOTPCode is the code for time t (for tests and tools).
func TOTPCode(secret []byte, t time.Time) string { return totpAt(secret, t.Unix()/totpPeriod) }
