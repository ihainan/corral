package web

import "testing"

func TestSidebarOrderSettingsValidation(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := admin.do("GET", "/api/me", nil).json()["id"]
	patch := func(seq int, set map[string]any) resp {
		return admin.do("PATCH", "/api/me/settings", map[string]any{
			"user":   uid,
			"writer": "sidebar-test",
			"seq":    seq,
			"set":    set,
		})
	}

	mustStatus(t, patch(1, map[string]any{
		"sidebarGroup":  "project",
		"sidebarSort":   "smart",
		"pinFixedOrder": true,
	}), 200, "valid sidebar settings")

	got := admin.do("GET", "/api/me/settings", nil).json()
	if got["sidebarGroup"] != "project" || got["sidebarSort"] != "smart" || got["pinFixedOrder"] != true {
		t.Fatalf("unexpected saved sidebar settings: %v", got)
	}

	mustStatus(t, patch(2, map[string]any{
		"sidebarGroup":  "session",
		"sidebarSort":   "name",
		"pinFixedOrder": false,
	}), 200, "update sidebar settings")

	for i, bad := range []map[string]any{
		{"sidebarGroup": "invalid"},
		{"sidebarSort": "oldest"},
		{"pinFixedOrder": "yes"},
		{"sidebarGroup": nil},
	} {
		mustStatus(t, patch(10+i, bad), 400, "invalid sidebar setting")
	}
}
