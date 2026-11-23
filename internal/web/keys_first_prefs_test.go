package web

import "testing"

// keysFirst (workbench 11-23): saved as one of its two words, anything else refused.
func TestKeysFirstSettingValidation(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := admin.do("GET", "/api/me", nil).json()["id"]
	patch := func(seq int, set map[string]any) resp {
		return admin.do("PATCH", "/api/me/settings", map[string]any{"user": uid, "writer": "keys-test", "seq": seq, "set": set})
	}
	mustStatus(t, patch(1, map[string]any{"keysFirst": "corral"}), 200, "valid keysFirst")
	if got := admin.do("GET", "/api/me/settings", nil).json(); got["keysFirst"] != "corral" {
		t.Fatalf("keysFirst not saved: %v", got)
	}
	for i, bad := range []any{"browser", true, nil} {
		mustStatus(t, patch(10+i, map[string]any{"keysFirst": bad}), 400, "invalid keysFirst")
	}
}
