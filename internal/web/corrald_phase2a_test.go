package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	consentOver24h := fixedNow.Add(-48 * time.Hour).Unix()

	// Seed 20 before-SSH (within 30d) and 20 after-corrald for "send",
	// plus only 19 before-SSH and 25 after-corrald for "card" (below threshold),
	// plus 30 SSH samples from 40 days before consent (must NOT be included in before window).
	oldBeforeDay := time.Unix(consentOver24h, 0).UTC().AddDate(0, 0, -40).Format("2006-01-02")
	validBeforeDay := time.Unix(consentOver24h, 0).UTC().AddDate(0, 0, -5).Format("2006-01-02")
	afterDay := time.Unix(consentOver24h, 0).UTC().AddDate(0, 0, 1).Format("2006-01-02")
	if err := h.srv.store.AddCorraldPerfDailyBatch(context.Background(), []store.CorraldPerfDailyDelta{
		{HostID: hostID, Day: oldBeforeDay, Op: "card", Via: "ssh", Count: 30, TotalUs: 90_000_000},
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

	// Enabled >= 24h: only "send" qualifies ("card" only has 19 samples in 30d window).
	setHostConsent(consentOver24h)
	r2 := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
	mustStatus(t, r2, http.StatusOK, "status over 24h")
	cmpList, ok := r2.json()["perf_comparison"].([]any)
	if !ok || len(cmpList) != 1 {
		t.Fatalf("expected 1 perf_comparison item (send only), got %#v", r2.json()["perf_comparison"])
	}
	item := cmpList[0].(map[string]any)
	if item["op"] != "send" || item["before_ms"].(float64) != 1900 || item["after_ms"].(float64) != 300 {
		t.Fatalf("unexpected send comparison item: %#v", item)
	}

	// Adding 1 more SSH card sample brings card to 20 -> now both send and card appear.
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

	// Since only this gateway was connected (connected_gateways == 1), the daemon
	// must be stopped on the host and host DB state marked remove_pending.
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := exec.Command(bin, "status")
		c.Env = append(os.Environ(), "HOME="+home)
		out, _ := c.CombinedOutput()
		if strings.Contains(string(out), "state=stopped") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected corrald process to stop after admin disable; status=%s", string(out))
		}
		time.Sleep(50 * time.Millisecond)
	}

	chRow, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
	if err != nil || chRow.State != gw.StateRemovePending {
		t.Fatalf("expected host DB state remove_pending, got (%+v, %v)", chRow, err)
	}

	// On next SSH connection (even while disabled), existing removal flow cleans
	// up host files and resets DB state to off.
	sshCl := dialTestSSH(t, sshSrv)
	defer sshCl.Close()
	h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshCl)

	chAfter, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
	if err != nil || chAfter.State != gw.StateOff {
		t.Fatalf("expected host state off after cleanup on reconnect, got (%+v, %v)", chAfter, err)
	}
	if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
		t.Fatalf("expected corrald binary symlink %s to be cleaned up, stat err=%v", bin, statErr)
	}

	// Re-enabling policy must NOT auto-reinstall; host stays off until user re-enables.
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

	// The daemon MUST still be running for the other gateway and files MUST stay intact.
	hAfter, err := otherClient.Health(context.Background())
	if err != nil || hAfter.ConnectedGateways != 1 {
		t.Fatalf("expected daemon still alive with 1 remaining gateway, got (%+v, %v)", hAfter, err)
	}
	bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
	if _, statErr := os.Stat(bin); statErr != nil {
		t.Fatalf("expected corrald files preserved, stat err=%v", statErr)
	}
	chMid, _ := h.srv.store.CorraldHostByID(context.Background(), hostID)
	if chMid.State == gw.StateRemovePending || chMid.State == gw.StateOff {
		t.Fatalf("expected host NOT marked remove_pending/off when other gateway connected, got %q", chMid.State)
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
