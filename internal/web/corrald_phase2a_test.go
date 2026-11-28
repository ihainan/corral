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
	"sync"
	"sync/atomic"
	"syscall"
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
		if chAfter.State != gw.StateRemovePending {
			t.Fatalf("expected host DB state remove_pending when clients.json unreadable, got %q", chAfter.State)
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

// Regression tests for Fix Round 4 end-to-end host/gateway interactions:
//   - Item #1 (CleanupUserHosts): uses atomic sole-gateway uninstall so another
//     gateway on the host is never killed or removed.
//   - Item #2: cancelled install (admin disable at install->start boundary)
//     uses sole-gateway uninstall and preserves another gateway's installation.
//   - Item #3: background allow-cleanup acquires per-account op lock and
//     re-reads DB state before touching SSH, so a concurrent explicit reinstall
//     is never disrupted; account-level dedup spans multiple host records.
//   - Item #8: admin-disabled OnHostConnected handles StateIncompatible hosts.
//   - Item #9: real attachAndHello publication synchronized with Close() under -race.
func TestCorraldFix4_EndToEndSafeCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)
	distV1, err := gw.LoadDist(v1Dir)
	if err != nil {
		t.Fatalf("LoadDist: %v", err)
	}

	t.Run("item2_cancelled_install_preserves_other_gateway_installation", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-cancel-a.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "cancel-shared")

		var once atomic.Bool
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-cancel-a.local",
			SSH:       &webSSHProvider{s: h.srv},
			BeforeInstallStart: func(hid int64, _ string) {
				if !once.CompareAndSwap(false, true) {
					return
				}
				// Right after install completes and before serve starts, another
				// gateway starts using the installation (writes clients.json with gw-b).
				bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
				serveCmd := exec.Command(bin, "serve", "--gateway", "gw-cancel-b.local")
				serveCmd.Env = append(os.Environ(), "HOME="+home)
				if out, err := serveCmd.CombinedOutput(); err != nil {
					t.Errorf("other gw serve: %v (%s)", err, string(out))
					return
				}
				stCmd := exec.Command(bin, "status")
				stCmd.Env = append(os.Environ(), "HOME="+home)
				stOut, _ := stCmd.CombinedOutput()
				sock := gw.ParseKeyValues(stOut)["socket"]
				if sock != "" {
					otherSSH := dialTestSSH(t, sshSrv)
					defer otherSSH.Close()
					if otherCl, derr := gw.Dial(context.Background(), otherSSH, sock, nil); derr == nil {
						_, _ = otherCl.Hello(context.Background(), proto.HelloParams{
							Gateway:        "gw-cancel-b.local",
							GatewayVersion: "0.3.2",
							ProtoMin:       1,
							ProtoMax:       1,
							Client:         proto.ClientIdentity{CorralUser: "bob", HostID: "99"},
						})
						_ = otherCl.Close()
					}
				}
				// Now admin disables corrald on gw-a -> sets rop.cancelled = true!
				h.srv.corrald.SetAdminDisabled(context.Background(), true)
			},
		})

		startCorraldInstall(t, admin, hostID)

		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			if err == nil && ch.State == gw.StateStopped {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected cancelled install with other gateway to mark stopped, got (%+v, %v)", ch, err)
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Other gateway's binary and running daemon MUST still be intact!
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("Item #2 regression: cancelled install deleted other gateway's binary: %v", err)
		}
		stCmd := exec.Command(bin, "status")
		stCmd.Env = append(os.Environ(), "HOME="+home)
		stOut, _ := stCmd.CombinedOutput()
		if !strings.Contains(string(stOut), "state=running") {
			t.Fatalf("Item #2 regression: cancelled install stopped other gateway's daemon; status=%s", string(stOut))
		}
		_ = adminID
	})

	t.Run("item3_background_allow_cleanup_never_deletes_reinstalled_daemon", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-reenable.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "reenable-box")
		hostID2, _ := createAndPinHost(t, admin, sshSrv, "reenable-box-peer")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Put hostID into remove_pending while disabled (hostID2 remains off on the same SSH account).
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		uCmd := exec.Command(bin, "uninstall")
		uCmd.Env = append(os.Environ(), "HOME="+home)
		_, _ = uCmd.CombinedOutput()
		if err := h.srv.store.ResetCorraldHostOff(context.Background(), hostID2); err != nil {
			t.Fatalf("ResetCorraldHostOff(hostID2): %v", err)
		}
		if err := h.srv.store.MarkCorraldHostsRemovePending(context.Background(), []int64{hostID}, fp, "alice"); err != nil {
			t.Fatalf("MarkCorraldHostsRemovePending: %v", err)
		}

		// Pause the allow-cleanup worker at BeforePendingCleanupSSH, verify it
		// holds the per-account op lock (StartInstall on hostID2 sharing the
		// same SSH account returns 409 Conflict while held), then simulate
		// another path resetting hostID to running BEFORE the worker reaches
		// SSH, and verify the resumed worker aborts without touching the new
		// installation!
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var hookOnce atomic.Bool
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:         h.store,
			Dist:          distV1,
			GatewayID:     "gw-reenable.local",
			AdminDisabled: true,
			SSH:           &webSSHProvider{s: h.srv},
			BeforePendingCleanupSSH: func(hid int64) {
				if hookOnce.CompareAndSwap(false, true) {
					entered <- struct{}{}
					<-release
				}
			},
		})

		mustStatus(t, admin.do("PUT", "/api/admin/corrald", map[string]any{"allowed": true}), http.StatusOK, "admin allow")
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("allow-cleanup worker did not reach BeforePendingCleanupSSH")
		}

		// While cleanup holds the per-account op lock, StartInstall on hostID2
		// (same SSH account, currently StateOff) MUST conflict with 409!
		st2 := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID2), nil)
		mustStatus(t, st2, http.StatusOK, "status hostID2")
		mhash, _ := st2.json()["manifest_hash"].(string)
		enableResp := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/install", hostID2), map[string]any{"manifest_hash": mhash})
		if enableResp.Status != http.StatusConflict {
			close(release)
			t.Fatalf("Item #3 regression: expected 409 Conflict while cleanup holds per-account op lock, got %d (%s)", enableResp.Status, enableResp.Body)
		}

		// Simulate state already having transitioned to StateRunning (e.g. a new
		// installation succeeded) while this allow worker was waiting.
		if err := h.srv.store.UpsertCorraldHost(context.Background(), &store.CorraldHost{
			HostID:           hostID,
			State:            gw.StateRunning,
			BoundFingerprint: fp,
			BoundUsername:    "alice",
			BoundGateway:     "gw-reenable.local",
			Version:          distV1.Version,
			Release:          distV1.Release,
		}); err != nil {
			close(release)
			t.Fatalf("UpsertCorraldHost: %v", err)
		}
		// Place a sentinel file at binary path to verify resumed worker does NOT uninstall it.
		if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
			close(release)
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("new-install"), 0o700); err != nil {
			close(release)
			t.Fatal(err)
		}

		close(release)
		time.Sleep(300 * time.Millisecond)

		chAfter, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if err != nil || chAfter.State != gw.StateRunning {
			t.Fatalf("Item #3 regression: resumed allow-cleanup altered StateRunning host to (%+v, %v)", chAfter, err)
		}
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("Item #3 regression: resumed allow-cleanup deleted new installation: %v", err)
		}
	})

	t.Run("item8_admin_disabled_connect_cleans_incompatible_host", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-incompat.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "incompat-box")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Stop daemon, mark host as StateIncompatible in DB, and restart Manager
		// with AdminDisabled=true.
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		stopCmd := exec.Command(bin, "stop")
		stopCmd.Env = append(os.Environ(), "HOME="+home)
		_, _ = stopCmd.CombinedOutput()
		waitDaemonStopped(t, home)

		h.srv.corrald.Close()
		if err := h.srv.store.UpsertCorraldHost(context.Background(), &store.CorraldHost{
			HostID:           hostID,
			State:            gw.StateIncompatible,
			BoundFingerprint: fp,
			BoundUsername:    "alice",
			BoundGateway:     "gw-incompat.local",
			Version:          "9.9.9",
			Release:          999999,
		}); err != nil {
			t.Fatalf("UpsertCorraldHost incompatible: %v", err)
		}
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:         h.store,
			Dist:          distV1,
			GatewayID:     "gw-incompat.local",
			AdminDisabled: true,
			SSH:           &webSSHProvider{s: h.srv},
		})

		sshCl := dialTestSSH(t, sshSrv)
		defer sshCl.Close()
		h.srv.corrald.OnHostConnected(context.Background(), adminID, hostID, "admin", fp, "alice", sshCl)

		chAfter, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
		if err != nil || chAfter.State != gw.StateOff {
			t.Fatalf("Item #8 regression: expected StateIncompatible host cleaned up to off on disabled connect, got (%+v, %v)", chAfter, err)
		}
		if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
			t.Fatalf("Item #8 regression: expected incompatible host binary removed, stat err=%v", statErr)
		}
	})

	t.Run("item9_real_attach_publish_synchronized_with_close", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-close9.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, _ := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "close9-box")

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Pause host so Resume acquires an owned SSH connection and enters
		// onHostConnectedOwned -> attachAndHello with a non-nil release func.
		mustStatus(t, admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/pause", hostID), nil), http.StatusOK, "pause")
		waitCorraldState(t, admin, hostID, gw.StatePaused)

		hookHit := make(chan struct{}, 1)
		closeGo := make(chan struct{})
		var once atomic.Bool
		var acquireCalls atomic.Int32
		var releaseCalls atomic.Int32
		countingSSH := &countingSSHProvider{
			base: &webSSHProvider{s: h.srv},
			onAcquire: func() {
				acquireCalls.Add(1)
			},
			onRelease: func() {
				releaseCalls.Add(1)
			},
		}

		h.srv.corrald.Close()
		mgr := gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-close9.local",
			SSH:       countingSSH,
			AfterAttachConnsInsert: func(hid int64) {
				if once.CompareAndSwap(false, true) {
					hookHit <- struct{}{}
					<-closeGo
				}
			},
		})
		h.srv.corrald = mgr

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = mgr.Resume(context.Background(), adminID, hostID, "admin")
		}()
		go func() {
			defer wg.Done()
			select {
			case <-hookHit:
				close(closeGo)
				mgr.Close()
			case <-time.After(8 * time.Second):
				t.Error("timed out waiting for AfterAttachConnsInsert hook")
			}
		}()
		wg.Wait()

		if acq, rel := acquireCalls.Load(), releaseCalls.Load(); acq == 0 || acq != rel {
			t.Fatalf("Item #9 regression: SSH acquire/release mismatch during attach/Close race: acquired=%d released=%d", acq, rel)
		}
	})

	t.Run("item1_cleanup_user_hosts_uses_sole_gateway_uninstall", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-cu1.local"
		})
		admin := h.admin()
		alice := h.member(admin, "alice")
		aliceID := userIDOf(t, alice)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, alice, sshSrv, "cu-sole")

		startCorraldInstall(t, alice, hostID)
		waitCorraldState(t, alice, hostID, gw.StateRunning)

		res := h.srv.CorraldManager().CleanupUserHosts(context.Background(), aliceID, "alice")
		if res[hostID] != "removed" {
			t.Fatalf("expected sole-gateway CleanupUserHosts to remove host, got %v", res)
		}
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); !os.IsNotExist(err) {
			t.Fatalf("expected binary removed after sole-gateway CleanupUserHosts, stat err=%v", err)
		}
	})
}

type countingSSHProvider struct {
	base      gw.SSHProvider
	onAcquire func()
	onRelease func()
}

func (p *countingSSHProvider) AcquireSSH(ctx context.Context, ownerID, hostID int64) (*ssh.Client, func(), string, string, error) {
	cl, rel, fp, u, err := p.base.AcquireSSH(ctx, ownerID, hostID)
	if err != nil {
		return nil, nil, "", "", err
	}
	if p.onAcquire != nil {
		p.onAcquire()
	}
	var once sync.Once
	wrappedRel := func() {
		once.Do(func() {
			if rel != nil {
				rel()
			}
			if p.onRelease != nil {
				p.onRelease()
			}
		})
	}
	return cl, wrappedRel, fp, u, nil
}

func (p *countingSSHProvider) HostBinding(ctx context.Context, ownerID, hostID int64) (string, string, error) {
	return p.base.HostBinding(ctx, ownerID, hostID)
}

func (p *countingSSHProvider) UserLabel(ctx context.Context, userID int64) string {
	return p.base.UserLabel(ctx, userID)
}

// Regression tests for Fix Round 5 end-to-end flows:
//   - Item #2 [P1]: CleanupUserHosts holds the per-account op lock until AFTER
//     the conditional DB reset finishes, so a concurrent install on a peer host
//     cannot race in and get clobbered.
//   - Item #3 [P2]: Admin disable firing after RunServeWithRetry inside
//     runInstallWithSSH reuses the install's held op lock (never skipping
//     cleanup on ErrOpConflict and never resetting a failed cleanup to off).
func TestCorraldFix5_EndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, _ := buildTestCorraldDist(t)
	distV1, err := gw.LoadDist(v1Dir)
	if err != nil {
		t.Fatalf("LoadDist: %v", err)
	}

	t.Run("item2_cleanup_user_hosts_holds_lock_through_db_reset", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-fix5-cu.local"
		})
		admin := h.admin()
		alice := h.member(admin, "alice")
		bob := h.member(admin, "bob")
		aliceID := userIDOf(t, alice)
		sshSrv, _ := startCorraldSSH(t)

		aliceHost, _ := createAndPinHost(t, alice, sshSrv, "cu5-alice")
		bobHost, _ := createAndPinHost(t, bob, sshSrv, "cu5-bob")

		startCorraldInstall(t, alice, aliceHost)
		waitCorraldState(t, alice, aliceHost, gw.StateRunning)

		// Bob's host is off on the same SSH account (so Bob is not a peer blocking cleanup).
		if err := h.srv.store.ResetCorraldHostOff(context.Background(), bobHost); err != nil {
			t.Fatalf("ResetCorraldHostOff(bobHost): %v", err)
		}

		// Pause CleanupUserHosts right between uninstall and DB reset.
		inReset := make(chan struct{}, 1)
		releaseReset := make(chan struct{})
		var once sync.Once
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-fix5-cu.local",
			SSH:       &webSSHProvider{s: h.srv},
			BeforeCleanupUserDBReset: func(hid int64) {
				once.Do(func() {
					inReset <- struct{}{}
					<-releaseReset
				})
			},
		})

		doneCh := make(chan map[int64]string, 1)
		go func() {
			doneCh <- h.srv.CorraldManager().CleanupUserHosts(context.Background(), aliceID, "alice")
		}()

		select {
		case <-inReset:
		case <-time.After(10 * time.Second):
			t.Fatal("CleanupUserHosts did not reach BeforeCleanupUserDBReset")
		}

		// While CleanupUserHosts is about to reset DB, per-account op lock MUST
		// STILL be held so Bob cannot start a concurrent install and get clobbered!
		stBob := bob.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", bobHost), nil)
		mustStatus(t, stBob, http.StatusOK, "bob status")
		mhash, _ := stBob.json()["manifest_hash"].(string)
		resp := bob.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/install", bobHost), map[string]any{"manifest_hash": mhash})
		if resp.Status != http.StatusConflict {
			close(releaseReset)
			t.Fatalf("Fix5 Item #2 regression: expected 409 Conflict while CleanupUserHosts holds lock before DB reset, got %d (%s)", resp.Status, resp.Body)
		}

		close(releaseReset)
		res := <-doneCh
		if res[aliceHost] != "removed" {
			t.Fatalf("expected CleanupUserHosts removed, got %v", res)
		}
	})

	t.Run("item3a_install_post_serve_admin_disable_reuses_lock_and_cleans_up", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-fix5-inst.local"
		})
		admin := h.admin()
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "inst5-clean")

		var once sync.Once
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-fix5-inst.local",
			SSH:       &webSSHProvider{s: h.srv},
			AfterInstallServe: func(hid int64, _ string) {
				once.Do(func() {
					// Admin disables right after corrald serve succeeds while
					// runInstallWithSSH STILL holds the OpInstall lock!
					h.srv.corrald.SetAdminDisabled(context.Background(), true)
				})
			},
		})

		startCorraldInstall(t, admin, hostID)

		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			_, binErr := os.Stat(bin)
			if err == nil && ch.State == gw.StateOff && os.IsNotExist(binErr) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Fix5 Item #3 regression: post-serve admin disable did not clean up while holding install op lock; ch=(%+v,%v) binErr=%v", ch, err, binErr)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("item3b_install_post_serve_disable_transient_fail_marks_remove_pending", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-fix5-inst2.local"
		})
		admin := h.admin()
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "inst5-pend")

		var failProbe atomic.Bool
		var once sync.Once
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-fix5-inst2.local",
			SSH:       &webSSHProvider{s: h.srv},
			ProbeOverride: func(ctx context.Context, c *ssh.Client) (*gw.HostProbe, error) {
				if failProbe.Load() {
					return nil, errors.New("simulated transient probe failure after serve")
				}
				return gw.ProbeHost(ctx, c)
			},
			AfterInstallServe: func(hid int64, _ string) {
				once.Do(func() {
					failProbe.Store(true)
					h.srv.corrald.SetAdminDisabled(context.Background(), true)
				})
			},
		})

		startCorraldInstall(t, admin, hostID)

		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			if err == nil && ch.State == gw.StateRemovePending {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Fix5 Item #3 regression: expected StateRemovePending when post-serve disable cleanup fails transiently, got (%+v, %v)", ch, err)
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Clean up the running daemon left on temp home.
		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		if _, err := os.Stat(bin); err == nil {
			stopCmd := exec.Command(bin, "stop")
			stopCmd.Env = append(os.Environ(), "HOME="+home)
			_, _ = stopCmd.CombinedOutput()
		}
	})
}

// Regression tests for Fix Round 7 Items #1, #2, #3, #5, #8:
//   - Item #1: no-binary tombstone recovery uploads and runs the Go binary under
//     .corrald-shared.lock -> op.lock -> corrald.lock (serializing with install).
//   - Item #2: admin disable during an in-flight update leaves remove_pending +
//     pending flag (or cleans up), never stopped with files and no retry.
//   - Item #3: existing-install reuse branch disabled right after Hello uses a
//     non-cancelled context and never ends in off while files/daemon remain.
//   - Item #5: partial recovery preserves the removing tombstone for retry.
//   - Item #8: leftovers in ~/.local/state/corral/run report partial (not removed).
func TestCorraldFix7_EndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	v1Dir, v2Dir := buildTestCorraldDist(t)
	distV1, err := gw.LoadDist(v1Dir)
	if err != nil {
		t.Fatalf("LoadDist v1: %v", err)
	}
	distV2, err := gw.LoadDist(v2Dir)
	if err != nil {
		t.Fatalf("LoadDist v2: %v", err)
	}

	t.Run("item1_5_8_no_binary_tombstone_recovery_locked_go_and_run_dir", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0500 does not block root")
		}
		sshSrv, home := startCorraldSSH(t)
		sshCl := dialTestSSH(t, sshSrv)
		defer sshCl.Close()

		// Probe the fresh home to get exact libDir/stateDir/machineID.
		probe, err := gw.ProbeHost(context.Background(), sshCl)
		if err != nil {
			t.Fatalf("ProbeHost: %v", err)
		}
		libDir := probe.LibDir
		stateDir := probe.StateDir
		runDir := filepath.Join(home, ".local", "state", "corral", "run")
		if err := os.MkdirAll(libDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatal(err)
		}
		// Simulate binaries already removed, leaving valid removing tombstone +
		// .corrald-shared.lock + a leftover file inside read-only run/.
		tombPath := filepath.Join(stateDir, "removing")
		if err := os.WriteFile(tombPath, []byte(`{"created_at":"2026-11-28T00:00:00Z","gateway":"gw-fix7.local"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sharedLockPath := filepath.Join(libDir, ".corrald-shared.lock")
		if err := os.WriteFile(sharedLockPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		runFile := filepath.Join(runDir, "corral-send-tmux-_0")
		if err := os.WriteFile(runFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// Lock runDir read-only so first recovery fails partially on run/ (Items #5 & #8).
		if err := os.Chmod(runDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(runDir, 0o700) })

		// Hold .corrald-shared.lock concurrently to prove Go recovery blocks on
		// .corrald-shared.lock (Item #1)!
		lockFD, err := os.OpenFile(sharedLockPath, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscallFlockEx(lockFD); err != nil {
			_ = lockFD.Close()
			t.Fatalf("flock shared lock: %v", err)
		}

		outCh := make(chan gw.UninstallOutcome, 1)
		go func() {
			outCh <- gw.RunUninstallSoleGatewayWithRetry(context.Background(), sshCl, distV1, probe, "op7rec1", "gw-fix7.local", gw.DefaultBusyRetryPolicy, nil)
		}()

		select {
		case out := <-outCh:
			_ = lockFD.Close()
			t.Fatalf("Fix7 Item #1 regression: no-binary recovery did NOT wait on .corrald-shared.lock! out=%+v", out)
		case <-time.After(200 * time.Millisecond):
		}
		_ = lockFD.Close()

		// First attempt must report partial because run/ could not be removed (Item #8),
		// AND must preserve the removing tombstone for retry (Item #5)!
		out1 := <-outCh
		if out1.Result != "partial" {
			t.Fatalf("Fix7 Item #8 regression: expected result=partial when run/ cannot be deleted, got %+v", out1)
		}
		if _, err := os.Stat(tombPath); err != nil {
			t.Fatalf("Fix7 Item #5 regression: removing tombstone was deleted on partial recovery failure: %v", err)
		}

		// Clear fault -> second retry completes cleanly to result=removed and removes run/ + stateDir + temp recovery dir!
		if err := os.Chmod(runDir, 0o700); err != nil {
			t.Fatal(err)
		}
		out2 := gw.RunUninstallSoleGatewayWithRetry(context.Background(), sshCl, distV1, probe, "op7rec2", "gw-fix7.local", gw.DefaultBusyRetryPolicy, nil)
		if out2.Result != "removed" {
			t.Fatalf("expected retry to succeed with result=removed, got %+v", out2)
		}
		if _, err := os.Lstat(runDir); !os.IsNotExist(err) {
			t.Fatalf("expected run/ removed, stat err=%v", err)
		}
		if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
			t.Fatalf("expected stateDir removed, stat err=%v", err)
		}
		if _, err := os.Lstat(filepath.Join(home, ".local", "state", "corral", ".rm-op7rec2")); !os.IsNotExist(err) {
			t.Fatalf("expected temp recovery dir removed, stat err=%v", err)
		}
	})

	t.Run("item2_disable_during_inflight_update_never_leaves_stopped_with_files", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-fix7-upd.local"
		})
		admin := h.admin()
		adminID := userIDOf(t, admin)
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "upd-disable")
		fp := hostFingerprint(t, h, adminID, hostID)

		startCorraldInstall(t, admin, hostID)
		waitCorraldState(t, admin, hostID, gw.StateRunning)

		// Recreate manager with v2 dist + BeforeUpdateStage hook that disables
		// corrald while failing probe transiently (so immediate cleanup cannot
		// complete during update exit — must record remove_pending, NOT stopped!).
		var failProbe atomic.Bool
		var once sync.Once
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV2,
			GatewayID: "gw-fix7-upd.local",
			SSH:       &webSSHProvider{s: h.srv},
			ProbeOverride: func(ctx context.Context, c *ssh.Client) (*gw.HostProbe, error) {
				if failProbe.Load() {
					return nil, errors.New("simulated transient probe failure during update disable")
				}
				return gw.ProbeHost(ctx, c)
			},
			BeforeUpdateStage: func(hid int64, _ string) {
				once.Do(func() {
					failProbe.Store(true)
					h.srv.corrald.SetAdminDisabled(context.Background(), true)
				})
			},
		})

		// Trigger update explicitly via POST /api/hosts/{id}/corrald/update.
		st := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil)
		mustStatus(t, st, http.StatusOK, "status before update")
		mhash, _ := st.json()["manifest_hash"].(string)
		ur := admin.do("POST", fmt.Sprintf("/api/hosts/%d/corrald/update", hostID), map[string]any{"manifest_hash": mhash})
		mustStatus(t, ur, http.StatusAccepted, "start update")

		// Host MUST settle in StateRemovePending (NEVER StateStopped!).
		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			if err == nil && ch.State == gw.StateRemovePending && ch.ActiveOpID == "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Fix7 Item #2 regression: expected StateRemovePending after disable during in-flight update, got (%+v, %v)", ch, err)
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Once transient probe error clears, existing-conn retry cleans up cleanly to off!
		failProbe.Store(false)
		existingSSH := dialTestSSH(t, sshSrv)
		defer existingSSH.Close()
		h.srv.corrald.MaybeRetryPendingCleanup(adminID, hostID, "admin", existingSSH)

		bin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		deadline = time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			_, binErr := os.Stat(bin)
			if err == nil && ch.State == gw.StateOff && os.IsNotExist(binErr) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected retry after update-disable to clean up to off; ch=(%+v,%v) binErr=%v", ch, err, binErr)
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = fp
	})

	t.Run("item3_existing_install_reuse_disable_after_hello_never_writes_off_with_files", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.CorraldDir = v1Dir
			c.GatewayID = "gw-fix7-reuse.local"
		})
		admin := h.admin()
		sshSrv, home := startCorraldSSH(t)
		hostID, _ := createAndPinHost(t, admin, sshSrv, "reuse-disable")

		// Pre-install and start corrald v1 directly on host (NO local DB row yet,
		// so StartInstall hits the existing-install reuse branch at manager.go:2173!).
		v1Bin := filepath.Join(v1Dir, "linux-"+runtime.GOARCH, "corrald")
		stOut, err := func() ([]byte, error) {
			cmd := exec.Command(v1Bin, "selftest")
			cmd.Env = append(os.Environ(), "HOME="+home)
			return cmd.CombinedOutput()
		}()
		if err != nil {
			t.Fatalf("selftest: %v (%s)", err, stOut)
		}
		stDir := gw.ParseKeyValues(stOut)["state_dir"]
		stageDir := filepath.Join(stDir, "stage-pre0001")
		_ = os.MkdirAll(stageDir, 0o700)
		b1, _ := os.ReadFile(v1Bin)
		_ = os.WriteFile(filepath.Join(stageDir, "corrald"), b1, 0o700)
		instCmd := exec.Command(filepath.Join(stageDir, "corrald"), "install", "--stage", stageDir, "--op", "pre0001", "--gateway", "gw-fix7-reuse.local", "--by", "admin")
		instCmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := instCmd.CombinedOutput(); err != nil {
			t.Fatalf("pre-install: %v (%s)", err, out)
		}
		installedBin := filepath.Join(home, ".local", "lib", "corral", "corrald")
		serveCmd := exec.Command(installedBin, "serve", "--gateway", "gw-fix7-reuse.local")
		serveCmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := serveCmd.CombinedOutput(); err != nil {
			t.Fatalf("pre-serve: %v (%s)", err, out)
		}

		// Trigger SetAdminDisabled(true) right after Hello inside attachAndHello
		// (which cancels install's opCtx!) while also failing probe transiently
		// so immediate uninstall cannot finish -> MUST end in StateRemovePending
		// (NEVER StateOff while binary/daemon remain on host!).
		var failProbe atomic.Bool
		var once sync.Once
		h.srv.corrald.Close()
		h.srv.corrald = gw.NewManager(gw.ManagerConfig{
			Store:     h.store,
			Dist:      distV1,
			GatewayID: "gw-fix7-reuse.local",
			SSH:       &webSSHProvider{s: h.srv},
			ProbeOverride: func(ctx context.Context, c *ssh.Client) (*gw.HostProbe, error) {
				if failProbe.Load() {
					return nil, errors.New("simulated transient probe failure after hello")
				}
				return gw.ProbeHost(ctx, c)
			},
			AfterAttachHello: func(hid int64) {
				once.Do(func() {
					failProbe.Store(true)
					h.srv.corrald.SetAdminDisabled(context.Background(), true)
				})
			},
		})

		startCorraldInstall(t, admin, hostID)

		deadline := time.Now().Add(10 * time.Second)
		for {
			ch, err := h.srv.store.CorraldHostByID(context.Background(), hostID)
			if err == nil && ch.State == gw.StateRemovePending && ch.ActiveOpID == "" {
				break
			}
			if err == nil && ch.State == gw.StateOff {
				if _, statErr := os.Stat(installedBin); statErr == nil {
					t.Fatalf("Fix7 Item #3 regression: host transitioned to StateOff while binary %s still exists!", installedBin)
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("Fix7 Item #3 regression: expected StateRemovePending when reuse-install disabled after Hello, got (%+v, %v)", ch, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

func syscallFlockEx(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
