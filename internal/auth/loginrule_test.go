package auth

import (
	"testing"
	"time"
)

// The rule tells a login valid as validLogin does: seen at a whole second,
// valid while now minus it is at most the limit, not after (review523-3).
func TestLoginRuleRoundsUp(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	for _, c := range []struct {
		now       time.Time
		seen      int64
		wantValid bool
	}{
		{base.Add(time.Hour), 1_000_000, true},                         // exactly the limit
		{base.Add(time.Hour + 500*time.Millisecond), 1_000_000, false}, // half a second past
		{base.Add(time.Hour + 500*time.Millisecond), 1_000_001, true},
	} {
		s := &Service{idle: time.Hour, maxAge: 24 * time.Hour, now: func() time.Time { return c.now }}
		r := s.LoginRule()
		valid := c.seen >= r.SeenSince
		byValidLogin := !(c.now.Sub(time.Unix(c.seen, 0)) > s.idle)
		if valid != c.wantValid || valid != byValidLogin {
			t.Errorf("now %v seen %d: rule %v, validLogin %v, want %v", c.now, c.seen, valid, byValidLogin, c.wantValid)
		}
	}
}
