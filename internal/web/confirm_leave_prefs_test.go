package web

import "testing"

// confirmLeave: saved as a boolean, anything else refused.
func TestConfirmLeaveSettingValidation(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := admin.do("GET", "/api/me", nil).json()["id"]
	patch := func(seq int, set map[string]any) resp {
		return admin.do("PATCH", "/api/me/settings", map[string]any{"user": uid, "writer": "leave-test", "seq": seq, "set": set})
	}
	mustStatus(t, patch(1, map[string]any{"confirmLeave": false}), 200, "valid confirmLeave false")
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["confirmLeave"] != false {
		t.Fatalf("confirmLeave=false not saved: %v", got)
	}
	mustStatus(t, patch(2, map[string]any{"confirmLeave": true}), 200, "valid confirmLeave true")
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["confirmLeave"] != true {
		t.Fatalf("confirmLeave=true not saved: %v", got)
	}
	for i, bad := range []any{"true", 1, nil} {
		mustStatus(t, patch(10+i, map[string]any{"confirmLeave": bad}), 400, "invalid confirmLeave")
	}
}
