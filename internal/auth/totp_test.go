package auth

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B (SHA-1), last six digits.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpAt(secret, ts/totpPeriod); got != want {
			t.Errorf("t=%d: %s, want %s", ts, got, want)
		}
	}
}

func TestCheckTOTPWindowAndReuse(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	step := now.Unix() / totpPeriod
	code := totpAt(secret, step)
	got, ok := CheckTOTP(secret, code, now, 0)
	if !ok || got != step {
		t.Fatalf("current code: %d %v", got, ok)
	}
	if _, ok := CheckTOTP(secret, code, now, step); ok {
		t.Fatal("a used code was accepted again")
	}
	if _, ok := CheckTOTP(secret, totpAt(secret, step-1), now, 0); !ok {
		t.Fatal("previous step refused")
	}
	if _, ok := CheckTOTP(secret, totpAt(secret, step-2), now, 0); ok {
		t.Fatal("code two steps old accepted")
	}
	if _, ok := CheckTOTP(secret, "12345", now, 0); ok {
		t.Fatal("short code accepted")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := NewRecoveryCodes()
	if len(codes) != recoveryCount || len(hashes) != recoveryCount {
		t.Fatal("count")
	}
	c := codes[0]
	if len(c) != 11 || c[5] != '-' || !looksLikeRecovery(c) || looksLikeRecovery("123456") {
		t.Fatalf("format %q", c)
	}
	if RecoveryHash(strings.ToUpper(strings.ReplaceAll(c, "-", " "))) != hashes[0] {
		t.Fatal("typed variant does not match")
	}
	uri := TOTPURI([]byte("12345678901234567890"), "Corral", "alice@example.org")
	if !strings.HasPrefix(uri, "otpauth://totp/Corral:alice@example.org?") || !strings.Contains(uri, "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ") {
		t.Fatalf("uri %s", uri)
	}
}
