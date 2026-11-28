package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

func dialTestSSH(t *testing.T, srv *testutil.SSHServer) *ssh.Client {
	t.Helper()
	cl, err := ssh.Dial("tcp", srv.Addr.String(), &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.Password("alice-pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh.Dial(%s): %v", srv.Addr.String(), err)
	}
	return cl
}

func hostFingerprint(t *testing.T, h *harness, ownerID, hostID int64) string {
	t.Helper()
	k, err := h.srv.store.HostKey(context.Background(), ownerID, hostID)
	if err != nil || k == nil || k.Fingerprint == "" {
		t.Fatalf("HostKey(%d): (%+v, %v)", hostID, k, err)
	}
	return k.Fingerprint
}

func userIDOf(t *testing.T, c *client) int64 {
	t.Helper()
	r := c.do("GET", "/api/me", nil)
	mustStatus(t, r, http.StatusOK, "GET /api/me")
	return int64(r.json()["id"].(float64))
}

func TestCorraldPerfDailyFlushPruneAndThresholds(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-test.local"
	})
	admin := h.admin()
	adminID := userIDOf(t, admin)
	sshSrv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, sshSrv, "perf-box")
	fp := hostFingerprint(t, h, adminID, hostID)

	// Control clock deterministically on perfDaily.
	fixedNow := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	h.srv.perfDaily.Close()
	h.srv.perfDaily = newCorraldPerfDaily(h.srv.store, h.srv.log, time.Hour, func() time.Time { return fixedNow })
	h.srv.corrald.Close()
	h.srv.initCorrald()

	// Record failed send (must NOT be persisted to daily rollups) and successful send/reply/card.
	h.srv.perfGatedVia("send", "ssh", time.Now().Add(-2*time.Second), context.DeadlineExceeded, hostID)
	h.srv.perfGatedVia("send", "ssh", time.Now().Add(-1500*time.Millisecond), nil, hostID)
	h.srv.perfGatedVia("reply", "ssh", time.Now().Add(-500*time.Millisecond), nil, hostID)
	h.srv.perfGatedVia("card", "corrald", time.Now().Add(-300*time.Millisecond), nil, hostID)

	if err := h.srv.perfDaily.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Verify /api/admin/perf still records gated.send / reply / card unchanged.
	pr := admin.do("GET", "/api/admin/perf", nil)
	mustStatus(t, pr, http.StatusOK, "admin perf")
	ops := pr.json()["ops"].(map[string]any)
	if ops["gated.send"] == nil || ops["gated.reply"] == nil || ops["gated.card"] == nil {
		t.Fatalf("expected /api/admin/perf to keep gated.send/reply/card unchanged: %+v", ops)
	}

	// Verify daily rollup merged send + reply into "send" (count=2, failed excluded).
	rows, err := h.srv.store.CorraldPerfDailyRows(context.Background(), hostID)
	if err != nil {
		t.Fatalf("CorraldPerfDailyRows: %v", err)
	}
	var sendSSHCount, cardCorraldCount int64
	for _, r := range rows {
		if r.Op == "send" && r.Via == "ssh" {
			sendSSHCount += r.Count
		}
		if r.Op == "card" && r.Via == "corrald" {
			cardCorraldCount += r.Count
		}
	}
	if sendSSHCount != 2 || cardCorraldCount != 1 {
		t.Fatalf("unexpected persisted counts: send/ssh=%d card/corrald=%d rows=%+v", sendSSHCount, cardCorraldCount, rows)
	}

	// Insert an expired row (>90 days old) and verify Flush prunes it automatically.
	expiredDay := fixedNow.AddDate(0, 0, -95).Format("2006-01-02")
	if err := h.srv.store.AddCorraldPerfDailyBatch(context.Background(), []store.CorraldPerfDailyDelta{
		{HostID: hostID, Day: expiredDay, Op: "send", Via: "ssh", Count: 50, TotalUs: 50_000_000},
	}); err != nil {
		t.Fatalf("seed expired row: %v", err)
	}
	h.srv.perfDaily.mu.Lock()
	h.srv.perfDaily.lastPrune = time.Time{} // force prune check on next Flush
	h.srv.perfDaily.mu.Unlock()
	if err := h.srv.perfDaily.Flush(context.Background()); err != nil {
		t.Fatalf("Flush prune: %v", err)
	}
	rowsAfterPrune, _ := h.srv.store.CorraldPerfDailyRows(context.Background(), hostID)
	for _, r := range rowsAfterPrune {
		if r.Day == expiredDay {
			t.Fatalf("expected expired row %s to be pruned", expiredDay)
		}
	}

	// Prepare host as enabled (running, SSH verified) and test display thresholds.
	if err := h.srv.store.RecordSSHVerified(context.Background(), hostID, fp, "alice"); err != nil {
		t.Fatalf("RecordSSHVerified: %v", err)
	}
	consentUnder24h := fixedNow.Add(-23 * time.Hour).Unix()
	// Enabled just over 24 hours ago (consentDay = 2026-11-27, fixedNow = 2026-11-28 12:00 UTC).
	consentOver24h := fixedNow.Add(-(24*time.Hour + time.Minute)).Unix()

	// Seed:
	// - 30 SSH card samples from 40 days before consent (outside 30d window -> excluded)
	// - 25 SSH card samples ON consentDay itself (Review Item #6: enable day excluded from before!)
	// - 25 corrald card samples ON consentDay itself (Review Item #6: enable day excluded from after!)
	// - 20 before-SSH (consentDay-1) and 20 after-corrald (consentDay+1) for "send" -> qualifies
	// - 19 before-SSH (consentDay-1) and 25 after-corrald (consentDay+1) for "card" -> below threshold (<20)
	consentTime := time.Unix(consentOver24h, 0).UTC()
	oldBeforeDay := consentTime.AddDate(0, 0, -40).Format("2006-01-02")
	validBeforeDay := consentTime.AddDate(0, 0, -1).Format("2006-01-02")
	consentDay := consentTime.Format("2006-01-02")
	afterDay := consentTime.AddDate(0, 0, 1).Format("2006-01-02")
	if err := h.srv.store.AddCorraldPerfDailyBatch(context.Background(), []store.CorraldPerfDailyDelta{
		{HostID: hostID, Day: oldBeforeDay, Op: "card", Via: "ssh", Count: 30, TotalUs: 90_000_000},
		{HostID: hostID, Day: consentDay, Op: "card", Via: "ssh", Count: 25, TotalUs: 50_000_000},
		{HostID: hostID, Day: consentDay, Op: "card", Via: "corrald", Count: 25, TotalUs: 7_500_000},
		{HostID: hostID, Day: validBeforeDay, Op: "send", Via: "ssh", Count: 20, TotalUs: 38_000_000}, // avg 1900ms
		{HostID: hostID, Day: afterDay, Op: "send", Via: "corrald", Count: 20, TotalUs: 6_000_000},    // avg 300ms
		{HostID: hostID, Day: validBeforeDay, Op: "card", Via: "ssh", Count: 19, TotalUs: 38_000_000}, // <20
		{HostID: hostID, Day: afterDay, Op: "card", Via: "corrald", Count: 25, TotalUs: 10_000_000},   // >=20
	}); err != nil {
		t.Fatalf("seed threshold rows: %v", err)
	}

	setHostConsent := func(consentAt int64) {
		ch := &store.CorraldHost{
			HostID:           hostID,
			State:            gw.StateRunning,
			ConsentAt:        consentAt,
			BoundFingerprint: fp,
			BoundUsername:    "alice",
			BoundGateway:     h.srv.cfg.GatewayID,
			Version:          "0.3.1",
			Release:          1000,
		}
		if err := h.srv.store.UpsertCorraldHost(context.Background(), ch); err != nil {
			t.Fatalf("UpsertCorraldHost: %v", err)
		}
	}

	// Enabled < 24h: perf_comparison must NOT be returned.
	setHostConsent(consentUnder24h)
	r1 := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
	mustStatus(t, r1, http.StatusOK, "status under 24h")
	if raw := r1.json()["perf_comparison"]; raw != nil {
		t.Fatalf("expected no perf_comparison under 24h, got %v", raw)
	}

	// Enabled >= 24h (24h+1m): only "send" qualifies ("card" has only 19 samples strictly
	// before consentDay; its 25 SSH samples ON consentDay must NOT be counted).
	setHostConsent(consentOver24h)
	r2 := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
	mustStatus(t, r2, http.StatusOK, "status over 24h")
	cmpList, ok := r2.json()["perf_comparison"].([]any)
	if !ok || len(cmpList) != 1 {
		t.Fatalf("expected 1 perf_comparison item (send only; consentDay excluded), got %#v", r2.json()["perf_comparison"])
	}
	item := cmpList[0].(map[string]any)
	if item["op"] != "send" || item["before_ms"].(float64) != 1900 || item["after_ms"].(float64) != 300 {
		t.Fatalf("unexpected send comparison item: %#v", item)
	}

	// Adding 1 more SSH card sample on consentDay-1 brings card to 20 -> now both send and card appear.
	// Card's before_n must be 20 (excluding 25 SSH samples on consentDay and 30 from -40d), and
	// card's after_n must be 26 (25 seeded on afterDay + 1 flushed earlier on fixedNow, excluding 25 on consentDay).
	if err := h.srv.store.AddCorraldPerfDailyBatch(context.Background(), []store.CorraldPerfDailyDelta{
		{HostID: hostID, Day: validBeforeDay, Op: "card", Via: "ssh", Count: 1, TotalUs: 2_000_000},
	}); err != nil {
		t.Fatalf("add 20th card sample: %v", err)
	}
	r3 := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
	mustStatus(t, r3, http.StatusOK, "status with card=20")
	cmpList2, ok := r3.json()["perf_comparison"].([]any)
	if !ok || len(cmpList2) != 2 {
		t.Fatalf("expected 2 perf_comparison items (send + card), got %#v", r3.json()["perf_comparison"])
	}
	cardItem := cmpList2[1].(map[string]any)
	if cardItem["op"] != "card" || cardItem["before_ms"].(float64) != 2000 || cardItem["before_n"].(float64) != 20 || cardItem["after_n"].(float64) != 26 {
		t.Fatalf("expected card comparison to exclude consentDay (before=2000ms before_n=20 after_n=26), got %#v", cardItem)
	}
}

func TestCorraldUpdateNoticesAndChangelogAPI(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
	})
	admin := h.admin()
	adminID := userIDOf(t, admin)
	bob := h.member(admin, "bob")

	sshSrv1, _ := startCorraldSSH(t)
	sshSrv2, _ := startCorraldSSH(t)
	hid1, _ := createAndPinHost(t, admin, sshSrv1, "web-1")
	hid2, _ := createAndPinHost(t, admin, sshSrv2, "web-2")

	// Install v1 on web-1, then upgrade gateway dist to v2 and trigger live hot update.
	startCorraldInstall(t, admin, hid1)
	waitCorraldState(t, admin, hid1, gw.StateRunning)

	v2Dist, err := gw.LoadDist(v2Dir)
	if err != nil {
		t.Fatalf("LoadDist v2: %v", err)
	}
	h.srv.corrald.SetDist(v2Dist)
	fp1 := hostFingerprint(t, h, adminID, hid1)
	sshCl1 := dialTestSSH(t, sshSrv1)
	defer sshCl1.Close()
	h.srv.corrald.OnHostConnected(context.Background(), adminID, hid1, "admin", fp1, "alice", sshCl1)

	// Wait for hot update to v2 (0.3.2) to finish and record a persisted update notice.
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hid1), nil).json()
		op, _ := st["op"].(map[string]any)
		opState, _ := op["state"].(string)
		if st["state"] == gw.StateRunning && st["version"] == "0.3.2" && opState != "pending" && opState != "running" {
			break
		}
		if time.Now().After(deadline) {
			b, _ := json.Marshal(st)
			t.Fatalf("timed out waiting for auto-update to 0.3.2; last=%s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Also seed a failure notice on web-2 to test mixed success + failure retrieval.
	if err := h.srv.store.RecordCorraldUpdateNotices(context.Background(), []store.CorraldUpdateNotice{
		{UserID: adminID, HostID: hid2, OldVersion: "0.3.1", NewVersion: "0.3.2", NewRelease: 2000, Success: false},
	}); err != nil {
		t.Fatalf("RecordCorraldUpdateNotices failure: %v", err)
	}

	// Bob must see 0 notices.
	rb := bob.do("GET", "/api/corrald/notices", nil)
	mustStatus(t, rb, http.StatusOK, "bob notices")
	if len(rb.json()["notices"].([]any)) != 0 {
		t.Fatalf("expected 0 notices for bob, got %v", rb.json()["notices"])
	}

	// Admin sees the real auto-update success notice on web-1 + failure notice on web-2.
	ra := admin.do("GET", "/api/corrald/notices", nil)
	mustStatus(t, ra, http.StatusOK, "admin notices")
	list := ra.json()["notices"].([]any)
	if len(list) != 2 {
		t.Fatalf("expected 2 notices for admin (1 auto-update success + 1 failure), got %d: %#v", len(list), list)
	}
	var ids []int64
	var sawSuccess, sawFailure bool
	for _, raw := range list {
		m := raw.(map[string]any)
		ids = append(ids, int64(m["id"].(float64)))
		if m["success"] == true && m["host_name"] == "web-1" && m["old_version"] == "0.3.1" && m["new_version"] == "0.3.2" {
			sawSuccess = true
		}
		if m["success"] == false && m["host_name"] == "web-2" && m["old_version"] == "0.3.1" && m["new_version"] == "0.3.2" {
			sawFailure = true
		}
	}
	if !sawSuccess || !sawFailure {
		t.Fatalf("missing expected notices (sawSuccess=%v sawFailure=%v): %#v", sawSuccess, sawFailure, list)
	}

	// Mark seen -> subsequent GET returns empty (shown once only).
	ms := admin.do("POST", "/api/corrald/notices/seen", map[string]any{"ids": ids})
	mustStatus(t, ms, http.StatusOK, "mark notices seen")
	ra2 := admin.do("GET", "/api/corrald/notices", nil)
	mustStatus(t, ra2, http.StatusOK, "admin notices after seen")
	if len(ra2.json()["notices"].([]any)) != 0 {
		t.Fatalf("expected 0 notices after marking seen, got %v", ra2.json()["notices"])
	}

	// Changelog API with multi-version entries + empty version omitted.
	v2Dist.Changelog = []proto.ChangelogEntry{
		{Version: "0.3.3", Zh: nil, En: nil},
		{Version: "0.3.2", Zh: []string{"版本 0.3.2 说明。"}, En: []string{"Notes for 0.3.2."}},
		{Version: "0.3.1", Zh: []string{"版本 0.3.1 说明。"}, En: []string{"Notes for 0.3.1."}},
	}
	h.srv.corrald.SetDist(v2Dist)

	cs := admin.do("POST", "/api/corrald/changelog/seen", map[string]any{"version": "0.3.1", "release": 1000})
	mustStatus(t, cs, http.StatusOK, "mark changelog seen")

	cg := admin.do("GET", "/api/corrald/changelog?version=0.3.2", nil)
	mustStatus(t, cg, http.StatusOK, "get changelog")
	entries := cg.json()["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["version"] != "0.3.2" {
		t.Fatalf("expected only 0.3.2 changelog entry, got %#v", entries)
	}
}

func TestCorraldAdminPolicyStopAndCleanupSingleGateway(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
	})
	admin := h.admin()
	adminID := userIDOf(t, admin)
	member := h.member(admin, "member1")

	// Member cannot read or modify admin policy.
	mustStatus(t, member.do("GET", "/api/admin/corrald", nil), http.StatusForbidden, "member get policy")
	mustStatus(t, member.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusForbidden, "member put policy")

	// Admin reads default policy.
	pg := admin.do("GET", "/api/admin/corrald", nil)
	mustStatus(t, pg, http.StatusOK, "admin get default policy")
	if pg.json()["allowed"] != true || pg.json()["locked_by_env"] != false {
		t.Fatalf("unexpected default policy: %+v", pg.json())
	}

	// Install & run corrald on a real test SSH host.
	sshSrv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, sshSrv, "policy-single")
	fp := hostFingerprint(t, h, adminID, hostID)
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, gw.StateRunning)

	if _, ok := h.srv.CorraldClient(adminID, hostID, "pane.send"); !ok {
		t.Fatalf("expected CorraldClient available before disable")
	}

	// Admin disables corrald.
	pd := admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false})
	mustStatus(t, pd, http.StatusOK, "admin disable corrald")
	if pd.json()["allowed"] != false {
		t.Fatalf("expected allowed=false, got %+v", pd.json())
	}

	// Persisted in app_settings + audit log written.
	val, ok, err := h.srv.store.AppSetting(context.Background(), corraldPolicySettingKey)
	if err != nil || !ok || val != "disabled" {
		t.Fatalf("expected app_settings corrald_policy=disabled, got (%q, %v, %v)", val, ok, err)
	}
	auditResp := admin.do("GET", "/api/admin/audit?kind=hosts", nil)
	mustStatus(t, auditResp, http.StatusOK, "audit log")
	var foundPolicyAudit bool
	for _, raw := range auditResp.jsonArray() {
		if raw["action"] == "corrald.policy" {
			foundPolicyAudit = true
			break
		}
	}
	if !foundPolicyAudit {
		t.Fatalf("expected corrald.policy audit entry")
	}

	// Immediately falls back to SSH and status reports admin_disabled=true.
	if _, ok := h.srv.CorraldClient(adminID, hostID, "pane.send"); ok {
		t.Fatalf("expected CorraldClient to return false immediately after disable")
	}
	stDis := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil).json()
	if stDis["admin_disabled"] != true {
		t.Fatalf("expected admin_disabled=true in host status, got %+v", stDis)
	}

	// Since only this gateway was connected and an SSH connection is already held,
	// the daemon must be stopped AND files immediately uninstalled (state -> off)
	// without waiting for a new SSH connection.
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	deadline := time.Now().Add(10 * time.Second)
	for {
		chRow, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
		_, statErr := os.Stat(bin)
		if err == nil && chRow.State == gw.StateOff && os.IsNotExist(statErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected immediate cleanup to off + binary removed; row=(%+v, %v) statErr=%v", chRow, err, statErr)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Re-enabling policy must NOT auto-reinstall; host stays off until user re-enables.
	sshCl := dialTestSSH(t, sshSrv)
	defer sshCl.Close()
	pe := admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true})
	mustStatus(t, pe, http.StatusOK, "admin re-enable corrald")
	h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshCl)
	stAfter := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil).json()
	if stAfter["state"] != gw.StateOff || stAfter["admin_disabled"] != false {
		t.Fatalf("expected host to remain off after re-allowing policy, got %+v", stAfter)
	}
}

func TestCorraldAdminPolicyKeepWhenOtherGatewayConnected(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
	})
	admin := h.admin()
	adminID := userIDOf(t, admin)

	sshSrv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, sshSrv, "policy-shared")
	fp := hostFingerprint(t, h, adminID, hostID)
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, gw.StateRunning)

	// Open a second live client connection to the same corrald socket via SSH
	// to simulate another gateway actively using corrald (connected_gateways == 2).
	chRow, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
	if err != nil || chRow.SocketPath == "" {
		t.Fatalf("expected socket_path in corrald_hosts, got (%+v, %v)", chRow, err)
	}
	otherSSH := dialTestSSH(t, sshSrv)
	defer otherSSH.Close()
	otherClient, err := gw.Dial(context.Background(), otherSSH, chRow.SocketPath, nil)
	if err != nil {
		t.Fatalf("second gateway Dial: %v", err)
	}
	defer otherClient.Close()
	if _, err := otherClient.Hello(context.Background(), proto.HelloParams{
		Gateway:        "other-gw.example.com",
		GatewayVersion: "0.3.1",
		ProtoMin:       1,
		ProtoMax:       1,
		Client:         proto.ClientIdentity{CorralUser: "other", HostID: "99"},
	}); err != nil {
		t.Fatalf("second gateway Hello: %v", err)
	}
	health, err := otherClient.Health(context.Background())
	if err != nil || health.ConnectedGateways < 2 {
		t.Fatalf("expected connected_gateways >= 2, got (%+v, %v)", health, err)
	}

	// Admin disables corrald on THIS gateway.
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin disable")

	// This gateway immediately stops using corrald.
	if _, ok := h.srv.CorraldClient(adminID, hostID, "pane.send"); ok {
		t.Fatalf("expected CorraldClient false while disabled")
	}

	// Wait for the background disable worker to disconnect this gateway.
	deadline := time.Now().Add(5 * time.Second)
	for {
		hAfter, err := otherClient.Health(context.Background())
		chMid, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if err == nil && hAfter.ConnectedGateways == 1 && chMid != nil && chMid.State == gw.StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected daemon alive with 1 remaining gateway and host stopped; health=(%+v,%v) ch=%+v", hAfter, err, chMid)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The daemon MUST still be running for the other gateway and files MUST stay intact.
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	if _, statErr := os.Stat(bin); statErr != nil {
		t.Fatalf("expected corrald files preserved, stat err=%v", statErr)
	}

	// Reconnecting while still disabled must NOT delete files or attach.
	h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", otherSSH)
	if _, statErr := os.Stat(bin); statErr != nil {
		t.Fatalf("expected files still preserved while disabled, stat err=%v", statErr)
	}

	// Admin re-allows corrald -> reconnect restores running state without reinstalling!
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true}), http.StatusOK, "admin allow")
	h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", otherSSH)
	waitCorraldState(t, admin, hostID, gw.StateRunning)
	if _, ok := h.srv.CorraldClient(adminID, hostID, "pane.send"); !ok {
		t.Fatalf("expected CorraldClient restored after re-allowing and reconnecting")
	}
}

func TestCorraldAdminPolicyEnvOverride(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)

	t.Run("env_disabled_overrides_db_allowed", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.CorraldAdminDisabledEnv = true
			c.CorraldAdminDisabled = true
		})
		admin := h.admin()
		_ = h.srv.store.SetAppSetting(context.Background(), corraldPolicySettingKey, "allowed")

		r := admin.do("GET", "/api/admin/corrald", nil)
		mustStatus(t, r, http.StatusOK, "get env-locked disabled")
		if r.json()["allowed"] != false || r.json()["locked_by_env"] != true {
			t.Fatalf("unexpected policy: %+v", r.json())
		}
		put := admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true})
		mustStatus(t, put, http.StatusConflict, "put env-locked returns 409")
		if put.json()["error"] != "env_override" {
			t.Fatalf("expected env_override error, got %+v", put.json())
		}
	})

	t.Run("env_allowed_overrides_db_disabled", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.CorraldAdminDisabledEnv = true
			c.CorraldAdminDisabled = false
		})
		admin := h.admin()
		_ = h.srv.store.SetAppSetting(context.Background(), corraldPolicySettingKey, "disabled")

		r := admin.do("GET", "/api/admin/corrald", nil)
		mustStatus(t, r, http.StatusOK, "get env-locked allowed")
		if r.json()["allowed"] != true || r.json()["locked_by_env"] != true {
			t.Fatalf("unexpected policy: %+v", r.json())
		}
		put := admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false})
		mustStatus(t, put, http.StatusConflict, "put env-locked returns 409")
	})

	t.Run("config_load_detects_env_presence", func(t *testing.T) {
		t.Setenv("CORRAL_PUBLIC_URL", "http://127.0.0.1:8080")
		t.Setenv("CORRAL_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

		t.Setenv("CORRAL_CORRALD_DISABLED", "")
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load unset: %v", err)
		}
		if cfg.CorraldAdminDisabledEnv || cfg.CorraldAdminDisabled {
			t.Fatalf("expected env unset -> (false, false), got (%v, %v)", cfg.CorraldAdminDisabledEnv, cfg.CorraldAdminDisabled)
		}

		t.Setenv("CORRAL_CORRALD_DISABLED", "on")
		cfg, err = config.Load()
		if err != nil {
			t.Fatalf("config.Load on: %v", err)
		}
		if !cfg.CorraldAdminDisabledEnv || !cfg.CorraldAdminDisabled {
			t.Fatalf("expected env on -> (true, true), got (%v, %v)", cfg.CorraldAdminDisabledEnv, cfg.CorraldAdminDisabled)
		}

		t.Setenv("CORRAL_CORRALD_DISABLED", "off")
		cfg, err = config.Load()
		if err != nil {
			t.Fatalf("config.Load off: %v", err)
		}
		if !cfg.CorraldAdminDisabledEnv || cfg.CorraldAdminDisabled {
			t.Fatalf("expected env off -> (true, false), got (%v, %v)", cfg.CorraldAdminDisabledEnv, cfg.CorraldAdminDisabled)
		}
	})
}

func waitDaemonStopped(t *testing.T, home string) {
	t.Helper()
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := exec.Command(bin, "status")
		c.Env = append(os.Environ(), "HOME="+home)
		out, _ := c.CombinedOutput()
		if strings.Contains(string(out), "state=stopped") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for corrald to stop; status=%s", string(out))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func findClientsJSON(t *testing.T, home string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, ".local", "state", "corral", "corrald", "*", "clients.json"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("expected clients.json under %s, glob=(%v, %v)", home, matches, err)
	}
	return matches[0]
}

// Regression test for Review Item #1 [P1]:
// Before cleaning up a remove_pending host on next SSH connection while
// admin-disabled, gateway MUST re-check exclusivity over SSH (live daemon or
// clients.json). If another gateway is active/registered or clients.json cannot
// be read, it MUST NOT uninstall — it must mark the host stopped and preserve files.
func TestCorraldAdminDisableReconnectChecksExclusivity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)

	t.Run("live_other_gateway_prevents_cleanup_of_remove_pending", func(t *testing.T) {
		distV1, err := gw.LoadDist(v1Dir)
		if err != nil {
			t.Fatalf("LoadDist: %v", err)
		}
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-a.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "recheck-live")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Simulate immediate uninstall failing (host disconnected during disable)
		// so the daemon is stopped, files remain, and DB is remove_pending.
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		stopCmd := exec.Command(bin, "stop")
		stopCmd.Env = append(os.Environ(), "HOME="+home)
		_, _ = stopCmd.CombinedOutput()
		waitDaemonStopped(t, home)

		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:         h.store,
			Dist:          distV1,
			GatewayID:     "gw-a.local",
			AdminDisabled: true,
			SSH:           &webSSHProvider{s: h.srv},
		})
		if err := h.srv.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, "alice"); err != nil {
			t.Fatalf("MarkCorraldHostsRemovePending: %v", err)
		}

		// Before gateway A reconnects, gateway B starts the shared daemon and connects!
		serveCmd := exec.Command(bin, "serve", "--gateway", "gw-b.example.com")
		serveCmd.Env = append(os.Environ(), "HOME="+home)
		out, err := serveCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gw-b corrald serve failed: %v (%s)", err, string(out))
		}
		kv := gw.ParseKeyValues(out)
		sock := kv["socket"]
		if sock == "" {
			t.Fatalf("expected socket from corrald serve, got %s", string(out))
		}
		sshB := dialTestSSH(t, sshSrv)
		defer sshB.Close()
		clB, err := gw.Dial(context.Background(), sshB, sock, nil)
		if err != nil {
			t.Fatalf("gw-b Dial: %v", err)
		}
		defer clB.Close()
		if _, err := clB.Hello(context.Background(), proto.HelloParams{
			Gateway:        "gw-b.example.com",
			GatewayVersion: "0.3.1",
			ProtoMin:       1,
			ProtoMax:       1,
			Client:         proto.ClientIdentity{CorralUser: "bob", HostID: "99"},
		}); err != nil {
			t.Fatalf("gw-b Hello: %v", err)
		}

		// Now gateway A reconnects while still disabled -> MUST NOT stop or uninstall gw-b's daemon!
		sshA := dialTestSSH(t, sshSrv)
		defer sshA.Close()
		h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshA)

		if _, err := clB.Health(context.Background()); err != nil {
			t.Fatalf("gw-b daemon must still be alive after gw-a reconnect: %v", err)
		}
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("corrald binary must be preserved when gw-b is active: %v", err)
		}
		chAfter, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if chAfter.State != gw.StateStopped {
			t.Fatalf("expected host DB state stopped (files preserved), got %q", chAfter.State)
		}
	})

	t.Run("stopped_daemon_with_other_gateway_in_clients_json_preserves_files", func(t *testing.T) {
		distV1, err := gw.LoadDist(v1Dir)
		if err != nil {
			t.Fatalf("LoadDist: %v", err)
		}
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-a.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "recheck-clients")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		stopCmd := exec.Command(bin, "stop")
		stopCmd.Env = append(os.Environ(), "HOME="+home)
		_, _ = stopCmd.CombinedOutput()
		waitDaemonStopped(t, home)

		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:         h.store,
			Dist:          distV1,
			GatewayID:     "gw-a.local",
			AdminDisabled: true,
			SSH:           &webSSHProvider{s: h.srv},
		})
		if err := h.srv.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, "alice"); err != nil {
			t.Fatalf("MarkCorraldHostsRemovePending: %v", err)
		}

		// Add another gateway entry to clients.json on disk while daemon is stopped.
		clientsPath := findClientsJSON(t, home)
		raw, err := json.Marshal([]proto.ClientInfo{
			{Gateway: "gw-a.local", CorralUser: "admin", HostID: fmt.Sprintf("%d", hostID)},
			{Gateway: "gw-other.local", CorralUser: "carol", HostID: "7"},
		})
		if err != nil {
			t.Fatalf("marshal clients.json: %v", err)
		}
		if err := os.WriteFile(clientsPath, raw, 0o600); err != nil {
			t.Fatalf("write clients.json: %v", err)
		}

		sshA := dialTestSSH(t, sshSrv)
		defer sshA.Close()
		h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshA)

		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("corrald binary must be preserved when clients.json lists another gateway: %v", err)
		}
		chAfter, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if chAfter.State != gw.StateStopped {
			t.Fatalf("expected host DB state stopped, got %q", chAfter.State)
		}
	})

	t.Run("unreadable_clients_json_preserves_files", func(t *testing.T) {
		distV1, err := gw.LoadDist(v1Dir)
		if err != nil {
			t.Fatalf("LoadDist: %v", err)
		}
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-a.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "recheck-corrupt")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		stopCmd := exec.Command(bin, "stop")
		stopCmd.Env = append(os.Environ(), "HOME="+home)
		_, _ = stopCmd.CombinedOutput()
		waitDaemonStopped(t, home)

		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:         h.store,
			Dist:          distV1,
			GatewayID:     "gw-a.local",
			AdminDisabled: true,
			SSH:           &webSSHProvider{s: h.srv},
		})
		if err := h.srv.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, "alice"); err != nil {
			t.Fatalf("MarkCorraldHostsRemovePending: %v", err)
		}

		// Corrupt clients.json so exclusivity cannot be confirmed.
		clientsPath := findClientsJSON(t, home)
		if err := os.WriteFile(clientsPath, []byte("{corrupt-json"), 0o600); err != nil {
			t.Fatalf("corrupt clients.json: %v", err)
		}

		sshA := dialTestSSH(t, sshSrv)
		defer sshA.Close()
		h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshA)

		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("corrald binary must be preserved when clients.json is unreadable: %v", err)
		}
		chAfter, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if chAfter.State != gw.StateStopped {
			t.Fatalf("expected host DB state stopped when clients.json unreadable, got %q", chAfter.State)
		}
	})
}

// Regression test for Review Item #3 [P2]:
// A host that was NOT currently connected in Manager.conns when the admin
// disabled corrald (e.g. DB state running/stopped after gateway restart) must
// still be handled on its next SSH connection: sole gateway -> stop & clean up
// to off; shared gateway -> preserve daemon & files and mark stopped.
func TestCorraldAdminDisableDisconnectedHostOnLaterConnect(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)
	distV1, err := gw.LoadDist(v1Dir)
	if err != nil {
		t.Fatalf("LoadDist: %v", err)
	}

	t.Run("sole_gateway_disconnected_cleans_on_reconnect", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-disc.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "disc-sole")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Recreate Manager (simulating gateway restart / disconnected host not in m.conns)
		// while DB still records StateRunning and daemon is still running on host.
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-disc.local",
			SSH:       &webSSHProvider{s: h.srv},
		})

		// Admin disables while host is disconnected.
		mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "disable")

		// Later SSH connects -> must detect sole gateway, stop daemon, and uninstall files!
		sshCl := dialTestSSH(t, sshSrv)
		defer sshCl.Close()
		h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshCl)

		chAfter, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if chAfter.State != gw.StateOff {
			t.Fatalf("expected disconnected sole-gateway host to be cleaned up to off, got %q", chAfter.State)
		}
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
			t.Fatalf("expected corrald binary removed, stat err=%v", statErr)
		}
	})

	t.Run("shared_gateway_disconnected_preserves_on_reconnect", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-disc.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "disc-shared")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)
		chRow, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)

		// Connect a second gateway to the running daemon.
		otherSSH := dialTestSSH(t, sshSrv)
		defer otherSSH.Close()
		otherCl, err := gw.Dial(context.Background(), otherSSH, chRow.SocketPath, nil)
		if err != nil {
			t.Fatalf("other gateway Dial: %v", err)
		}
		defer otherCl.Close()
		if _, err := otherCl.Hello(context.Background(), proto.HelloParams{
			Gateway:        "gw-peer.example.com",
			GatewayVersion: "0.3.1",
			ProtoMin:       1,
			ProtoMax:       1,
			Client:         proto.ClientIdentity{CorralUser: "peer", HostID: "88"},
		}); err != nil {
			t.Fatalf("other gateway Hello: %v", err)
		}

		// Disconnect this gateway (recreate Manager -> not in m.conns), then admin disables.
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-disc.local",
			SSH:       &webSSHProvider{s: h.srv},
		})
		mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "disable")

		// Later SSH connects -> must detect other gateway, preserve daemon/files, mark stopped.
		sshCl := dialTestSSH(t, sshSrv)
		defer sshCl.Close()
		h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshCl)

		if _, err := otherCl.Health(context.Background()); err != nil {
			t.Fatalf("other gateway daemon must remain alive: %v", err)
		}
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("corrald binary must be preserved: %v", err)
		}
		chAfter, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if chAfter.State != gw.StateStopped {
			t.Fatalf("expected stopped state for shared disconnected host, got %q", chAfter.State)
		}
	})
}

// Regression test for Review Item #4 [P2]:
// An in-flight OnHostConnected / attachAndHello that already passed the entry
// policy check and races with SetAdminDisabled(true) MUST NOT establish a live
// connection or leave the host running; it must immediately enforce the disable
// rule and close.
func TestCorraldAdminDisableInFlightAttachRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)
	distV1, err := gw.LoadDist(v1Dir)
	if err != nil {
		t.Fatalf("LoadDist: %v", err)
	}
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-race.local"
	})
	admin := h.admin()
	adminID := userIDOf(t, admin)
	sshSrv, home := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, sshSrv, "race-box")
	fp := hostFingerprint(t, h, adminID, hostID)

	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, gw.StateRunning)

	// Recreate Manager with AfterAttachHello hook so we can deterministically
	// fire SetAdminDisabled(true) right after Dial+Hello completes and before
	// attachAndHello publishes the connection.
	inAttach := make(chan struct{})
	releaseAttach := make(chan struct{})
	var hookOnce atomic.Bool
	h.srv.corrald.Close()
	h.srv.corrald = gw.NewManager(gw.ManagerConfig{
		Store:     h.store,
		Dist:      distV1,
		GatewayID: "gw-race.local",
		SSH:       &webSSHProvider{s: h.srv},
		AfterAttachHello: func(hid int64) {
			if hid == hostID && hookOnce.CompareAndSwap(false, true) {
				close(inAttach)
				<-releaseAttach
			}
		},
	})

	sshCl := dialTestSSH(t, sshSrv)
	defer sshCl.Close()

	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshCl)
	}()

	select {
	case <-inAttach:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for attachAndHello to reach AfterAttachHello hook")
	}

	// Admin disables corrald while attachAndHello is in flight!
	mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin disable mid-attach")
	close(releaseAttach)

	select {
	case <-attachDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for OnHostConnected to finish")
	}

	// Must have ZERO live client, daemon stopped & files uninstalled immediately (state -> off).
	if _, ok := h.srv.CorraldClient(adminID, hostID, "pane.send"); ok {
		t.Fatalf("expected no CorraldClient after in-flight attach raced with admin disable")
	}
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	chAfter, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
	if err != nil || chAfter.State != gw.StateOff {
		t.Fatalf("expected DB state off after raced attach immediate cleanup, got (%+v, %v)", chAfter, err)
	}
	if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
		t.Fatalf("expected corrald binary removed after raced attach cleanup, stat err=%v", statErr)
	}
}

// Regression test for Review Item #7 [P2]:
// When a flush fails transiently concurrently with Close(), the failed batch
// MUST be re-queued even though p.closed is already true, and Close()'s final
// flush MUST persist it while ignoring any new Observe() calls made after Close.
func TestCorraldPerfDailyRequeueOnFailedFlushConcurrentWithClose(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	sshSrv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, sshSrv, "flush-race")

	fixedNow := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	p := newCorraldPerfDaily(h.srv.store, h.srv.log, time.Hour, func() time.Time { return fixedNow })

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	p.writeBatch = func(ctx context.Context, deltas []store.CorraldPerfDailyDelta) error {
		n := calls.Add(1)
		if n == 1 {
			close(firstEntered)
			<-releaseFirst
			return errors.New("transient sqlite busy")
		}
		return h.srv.store.AddCorraldPerfDailyBatch(ctx, deltas)
	}

	// Buffer 3 samples before close.
	p.Observe(hostID, "send", "corrald", 250*time.Millisecond)
	p.Observe(hostID, "send", "corrald", 350*time.Millisecond)
	p.Observe(hostID, "card", "ssh", 1500*time.Millisecond)

	// Start a flush that will fail transiently while Close() is called concurrently.
	flushErrCh := make(chan error, 1)
	go func() {
		flushErrCh <- p.Flush(context.Background())
	}()

	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first flush to enter writeBatch")
	}

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		p.Close()
	}()

	// Wait until Close() has set p.closed = true.
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		isClosed := p.closed
		p.mu.Unlock()
		if isClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Close() to mark closed=true")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// New samples observed after Close() starts must NOT be accepted.
	p.Observe(hostID, "send", "corrald", 999*time.Millisecond)

	// Let the first flush fail -> it must re-queue its batch despite p.closed==true,
	// and Close()'s final flush must then write it to SQLite.
	close(releaseFirst)

	if err := <-flushErrCh; err == nil {
		t.Fatalf("expected first flush to return transient error")
	}
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Close() to finish")
	}

	if calls.Load() < 2 {
		t.Fatalf("expected Close() to run a second flush retry, got %d writeBatch calls", calls.Load())
	}

	rows, err := h.srv.store.CorraldPerfDailyRows(context.Background(), hostID)
	if err != nil {
		t.Fatalf("CorraldPerfDailyRows: %v", err)
	}
	var sendCount, cardCount int64
	for _, r := range rows {
		if r.Op == "send" && r.Via == "corrald" {
			sendCount += r.Count
		}
		if r.Op == "card" && r.Via == "ssh" {
			cardCount += r.Count
		}
	}
	if sendCount != 2 || cardCount != 1 {
		t.Fatalf("expected persisted pre-close samples (send=2, card=1) and post-close sample dropped, got send=%d card=%d rows=%+v", sendCount, cardCount, rows)
	}
}

// Regression test for Fix Round 2 Item #1 [P1]:
// End-to-end verification of immediate cleanup on pooled SSH connections,
// non-uninstall when another gateway is registered, retry on next host use
// over an existing connection (without a new SSH dial), and re-allow
// reconciliation of remove_pending hosts without auto-reinstall.
func TestCorraldAdminDisableImmediateCleanupOnPooledSSH(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)
	distV1, err := gw.LoadDist(v1Dir)
	if err != nil {
		t.Fatalf("LoadDist: %v", err)
	}

	t.Run("sole_gateway_cleans_immediately_without_new_connection", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-pool.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "pooled-sole")

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Track whether corrald uninstall ran via a wrapper or filesystem check:
		// verify ~/.local/lib/corral/corrald and ~/.local/state/corral/corrald exist now.
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		stateCorrald := filepath.Join(home, ".local", "state", "corral", "corrald")
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("expected binary installed before disable: %v", err)
		}

		// Disable while keeping the existing connection alive (NO OnHostConnected after disable!).
		mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin disable")

		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			_, binErr := os.Stat(bin)
			_, stateErr := os.Stat(stateCorrald)
			if err == nil && ch.State == gw.StateOff && os.IsNotExist(binErr) && os.IsNotExist(stateErr) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected immediate cleanup to off without new connection; ch=(%+v,%v) binErr=%v stateErr=%v", ch, err, binErr, stateErr)
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = adminID
	})

	t.Run("other_gateway_registered_in_registry_never_uninstalls", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-pool.local"
		})
		admin := h.admin()
		sshSrv, home := startCorraldSSH(t)
		hid, _ := createAndPinHost(t, admin, sshSrv, "pooled-other-reg")
		startCorraldInstall(t, admin, hid)
		waitCorraldState(t, admin, hid, gw.StateRunning)
		chRow, _ := h.srv.store.CorraldHostByID(context.Background(), hid)

		// Another gateway connects, registers in ClientRegistry, and then DISCONNECTS
		// (so ConnectedGateways == 1, but clients.json still records gw-other!).
		otherSSH := dialTestSSH(t, sshSrv)
		otherCl, err := gw.Dial(context.Background(), otherSSH, chRow.SocketPath, nil)
		if err != nil {
			t.Fatalf("other gateway Dial: %v", err)
		}
		if _, err := otherCl.Hello(context.Background(), proto.HelloParams{
			Gateway:        "gw-other-registered.local",
			GatewayVersion: "0.3.1",
			ProtoMin:       1,
			ProtoMax:       1,
			Client:         proto.ClientIdentity{CorralUser: "bob", HostID: "42"},
		}); err != nil {
			t.Fatalf("other gateway Hello: %v", err)
		}
		_ = otherCl.Close()
		_ = otherSSH.Close()

		// Disable on gw-pool.local -> live Hello check sees gw-other-registered.local,
		// so it MUST NOT stop the daemon or uninstall files!
		mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": false}), http.StatusOK, "admin disable")

		deadline := time.Now().Add(5 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hid)
			if err == nil && ch.State == gw.StateStopped {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected stopped state when another gateway is registered, got (%+v, %v)", ch, err)
			}
			time.Sleep(50 * time.Millisecond)
		}

		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("expected binary preserved when another gateway is registered: %v", err)
		}
		stCmd := exec.Command(bin, "status")
		stCmd.Env = append(os.Environ(), "HOME="+home)
		out, _ := stCmd.CombinedOutput()
		if !strings.Contains(string(out), "state=running") {
			t.Fatalf("expected daemon still running when another gateway is registered; status=%s", string(out))
		}
	})

	t.Run("remove_pending_retries_on_existing_connection_agent_locate", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-retry.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "retry-existing-conn")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Stop daemon and leave host in remove_pending on a restarted Manager
		// (simulating immediate cleanup failure while files remain on host).
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		stopCmd := exec.Command(bin, "stop")
		stopCmd.Env = append(os.Environ(), "HOME="+home)
		_, _ = stopCmd.CombinedOutput()
		waitDaemonStopped(t, home)

		if err := h.srv.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, "alice"); err != nil {
			t.Fatalf("MarkCorraldHostsRemovePending: %v", err)
		}
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:         h.store,
			Dist:          distV1,
			GatewayID:     "gw-retry.local",
			AdminDisabled: true,
			SSH:           &webSSHProvider{s: h.srv},
		})

		// Use an already-open SSH connection on a terminal session (e.g. opening
		// the agents list -> tryCorraldLocate) WITHOUT calling OnHostConnected!
		existingSSH := dialTestSSH(t, sshSrv)
		defer existingSSH.Close()
		sess := newTestTermSession(t, adminID, hostID, "tmux", existingSSH)
		_, _ = h.srv.tryCorraldLocate(context.Background(), sess, proto.AgentLocateParams{Mode: "tmux"})

		// Background retry over the existing SSH connection must clean up files -> state off!
		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			_, binErr := os.Stat(bin)
			if err == nil && ch.State == gw.StateOff && os.IsNotExist(binErr) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected remove_pending retry on existing conn locate to clean up to off; ch=(%+v,%v) binErr=%v", ch, err, binErr)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("reallow_resets_already_cleaned_remove_pending_to_off_without_reinstall", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-reallow.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "reallow-cleaned")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Manually uninstall on host and force DB into remove_pending while disabled
		// (simulating files already gone before admin re-allows).
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		uCmd := exec.Command(bin, "uninstall")
		uCmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := uCmd.CombinedOutput(); err != nil {
			t.Fatalf("manual uninstall: %v (%s)", err, string(out))
		}

		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:         h.store,
			Dist:          distV1,
			GatewayID:     "gw-reallow.local",
			AdminDisabled: true,
			SSH:           &webSSHProvider{s: h.srv},
		})
		if err := h.srv.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, "alice"); err != nil {
			t.Fatalf("MarkCorraldHostsRemovePending: %v", err)
		}

		// Admin re-allows corrald -> reconcile finds files already gone and resets DB to off
		// without reinstalling corrald!
		mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true}), http.StatusOK, "admin allow")

		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			if err == nil && ch.State == gw.StateOff {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected re-allow to reset already-cleaned remove_pending host to off, got (%+v, %v)", ch, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
			t.Fatalf("re-allowing must NOT auto-reinstall corrald; stat err=%v", statErr)
		}
	})
}
