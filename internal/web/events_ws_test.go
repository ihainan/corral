//go:build linux

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/perf"
)

type wsPushMsg struct {
	T       string  `json:"t"`
	Session string  `json:"session,omitempty"`
	Pane    string  `json:"pane,omitempty"`
	Host    int64   `json:"host,omitempty"`
	Hosts   []int64 `json:"hosts,omitempty"`
}

func dialEventsWS(t *testing.T, h *harness, c *client, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := strings.Replace(h.ts.URL, "http", "ws", 1) + "/api/events/ws"
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	if c != nil && c.cookie != "" {
		hdr.Set("Cookie", h.srv.cookieName()+"="+c.cookie)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
}

func readPushMsg(t *testing.T, ws *websocket.Conn, timeout time.Duration) wsPushMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read ws push message: %v", err)
	}
	var m wsPushMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal ws push message %q: %v", data, err)
	}
	return m
}

func sendWatchHosts(t *testing.T, ws *websocket.Conn, hosts []int64) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"t": "watch", "hosts": hosts})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write watch message: %v", err)
	}
}

type wsPushReader struct {
	ws     *websocket.Conn
	ch     chan wsPushMsg
	closed chan struct{}
}

func startPushReader(ws *websocket.Conn) *wsPushReader {
	r := &wsPushReader{
		ws:     ws,
		ch:     make(chan wsPushMsg, 64),
		closed: make(chan struct{}),
	}
	go func() {
		defer close(r.closed)
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m wsPushMsg
			if json.Unmarshal(data, &m) == nil {
				r.ch <- m
			}
		}
	}()
	return r
}

func (r *wsPushReader) next(t *testing.T, timeout time.Duration) wsPushMsg {
	t.Helper()
	select {
	case m := <-r.ch:
		return m
	case <-r.closed:
		t.Fatal("ws push reader closed unexpectedly")
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for ws push message after %v", timeout)
	}
	return wsPushMsg{}
}

func (r *wsPushReader) expectNone(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case m := <-r.ch:
		t.Fatalf("expected no push message within %v, got %+v", wait, m)
	case <-time.After(wait):
	}
}

func TestEventsWSAuthAndOriginValidation(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	alice := h.member(admin, "alice")

	// 1. Unauthenticated -> 401.
	_, resp, err := dialEventsWS(t, h, &client{h: h}, h.cfg.Origin())
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated WS: err=%v status=%v, want 401", err, resp)
	}

	// 2. Wrong Origin -> 403 bad_origin.
	_, resp, err = dialEventsWS(t, h, alice, "https://evil.example")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong origin WS: err=%v status=%v, want 403", err, resp)
	}

	// 3. Missing Origin on WebSocket upgrade -> 403 bad_origin.
	_, resp, err = dialEventsWS(t, h, alice, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing origin WS: err=%v status=%v, want 403", err, resp)
	}

	// 4. Valid authenticated + same Origin -> connects and sends initial cover.
	ws, resp, err := dialEventsWS(t, h, alice, h.cfg.Origin())
	if err != nil {
		t.Fatalf("valid WS dial failed: %v (resp=%v)", err, resp)
	}
	defer ws.CloseNow()

	first := readPushMsg(t, ws, 2*time.Second)
	if first.T != "cover" || len(first.Hosts) != 0 {
		t.Fatalf("initial message = %+v, want {t:cover hosts:[]}", first)
	}

	// Ping/pong roundtrip.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"t":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	pong := readPushMsg(t, ws, 2*time.Second)
	if pong.T != "pong" {
		t.Fatalf("expected pong, got %+v", pong)
	}

	// 5. Disabling user immediately closes the push WebSocket.
	aliceID := int64(alice.do("GET", "/api/me", nil).json()["id"].(float64))
	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", aliceID), map[string]any{"disabled": true}), http.StatusOK, "disable alice")

	closed := make(chan struct{})
	go func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer rcancel()
		for {
			if _, _, rerr := ws.Read(rctx); rerr != nil {
				close(closed)
				return
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("events WS stayed open after user was disabled")
	}
}

func TestEventsHintMappingCoalescingIsolationAndPerf(t *testing.T) {
	h := newHarness(t)
	h.srv.events.SetCoalesceWindow(120 * time.Millisecond)

	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")

	srv := h.sshServer(t.TempDir())
	aliceHostID, aliceSessID := createAndPinHost(t, alice, srv, "alice-box")
	bobHostID, bobSessID := createAndPinHost(t, bob, srv, "bob-box")
	aliceID := int64(alice.do("GET", "/api/me", nil).json()["id"].(float64))
	bobID := int64(bob.do("GET", "/api/me", nil).json()["id"].(float64))

	// Bind Alice's session pane to a specific log & PID, and Bob's to another.
	aliceLog := "/tmp/alice-claude.jsonl"
	bobLog := "/tmp/bob-claude.jsonl"
	h.srv.events.RecordBinding(aliceID, aliceHostID, aliceSessID, "w1:p1", "w1:p1", 41001, aliceLog)
	h.srv.events.RecordBinding(bobID, bobHostID, bobSessID, "w1:p9", "w1:p9", 42002, bobLog)

	wsAlice, _, err := dialEventsWS(t, h, alice, h.cfg.Origin())
	if err != nil {
		t.Fatal(err)
	}
	defer wsAlice.CloseNow()
	rdAlice := startPushReader(wsAlice)
	_ = rdAlice.next(t, 2*time.Second) // initial cover

	wsBob, _, err := dialEventsWS(t, h, bob, h.cfg.Origin())
	if err != nil {
		t.Fatal(err)
	}
	defer wsBob.CloseNow()
	rdBob := startPushReader(wsBob)
	_ = rdBob.next(t, 2*time.Second) // initial cover

	// Alice watches her host; Bob tries to watch BOTH Alice's host (unauthorized!) and his own host.
	sendWatchHosts(t, wsAlice, []int64{aliceHostID})
	sendWatchHosts(t, wsBob, []int64{aliceHostID, bobHostID})
	time.Sleep(40 * time.Millisecond)

	// 1. Matched log change on Alice's host -> ONLY Alice receives {"t":"convo", session: aliceSessID, pane: "w1:p1"}.
	// Bob MUST receive NOTHING (even though he tried to watch aliceHostID!).
	h.srv.events.HandleAgentChanged([]int64{aliceHostID}, proto.AgentChangedEventData{
		ChangedLogs: []string{aliceLog},
	})
	msg := rdAlice.next(t, 2*time.Second)
	if msg.T != "convo" || msg.Session != aliceSessID || msg.Pane != "w1:p1" {
		t.Fatalf("alice got %+v, want convo(%s, w1:p1)", msg, aliceSessID)
	}
	rdBob.expectNone(t, 150*time.Millisecond)

	// 2. Shared-account isolation: event affects BOTH aliceHostID and bobHostID with aliceLog.
	// Alice gets convo(aliceSessID, w1:p1); Bob gets ONLY agents(bobHostID) — NEVER Alice's session/pane!
	time.Sleep(150 * time.Millisecond) // let previous coalesce window expire
	h.srv.events.HandleAgentChanged([]int64{aliceHostID, bobHostID}, proto.AgentChangedEventData{
		ChangedLogs: []string{aliceLog},
	})
	aMsg := rdAlice.next(t, 2*time.Second)
	if aMsg.T != "convo" || aMsg.Session != aliceSessID || aMsg.Pane != "w1:p1" {
		t.Fatalf("alice shared-account msg = %+v", aMsg)
	}
	bMsg := rdBob.next(t, 2*time.Second)
	if bMsg.T != "agents" || bMsg.Host != bobHostID || bMsg.Session != "" {
		t.Fatalf("bob must only receive agents for bobHostID, got %+v", bMsg)
	}

	// 3. Unmatched log path on Alice's host -> {"t":"agents","host":aliceHostID}.
	time.Sleep(150 * time.Millisecond)
	h.srv.events.HandleAgentChanged([]int64{aliceHostID}, proto.AgentChangedEventData{
		ChangedLogs: []string{"/tmp/unknown-agent.jsonl"},
	})
	aAgents := rdAlice.next(t, 2*time.Second)
	if aAgents.T != "agents" || aAgents.Host != aliceHostID {
		t.Fatalf("unmatched log expected agents(%d), got %+v", aliceHostID, aAgents)
	}

	// 4. Removed PID matching bound pane -> emits BOTH convo (for pane) and agents (for host).
	time.Sleep(150 * time.Millisecond)
	h.srv.events.HandleAgentChanged([]int64{aliceHostID}, proto.AgentChangedEventData{
		RemovedPIDs: []int{41001},
	})
	gotTypes := map[string]wsPushMsg{}
	for i := 0; i < 2; i++ {
		m := rdAlice.next(t, 2*time.Second)
		gotTypes[m.T] = m
	}
	if gotTypes["convo"].Session != aliceSessID || gotTypes["convo"].Pane != "w1:p1" {
		t.Fatalf("expected convo hint for exited bound PID, got %+v", gotTypes)
	}
	if gotTypes["agents"].Host != aliceHostID {
		t.Fatalf("expected agents hint for exited bound PID, got %+v", gotTypes)
	}

	// 5. Coalescing: 6 rapid hints for the same pane within 120ms window ->
	// 1 immediate leading push, 0 extra pushes during window, 1 trailing push at window end,
	// and >= 4 hints counted as coalesced in /api/admin/perf!
	time.Sleep(160 * time.Millisecond)
	for i := 0; i < 6; i++ {
		h.srv.events.HandleAgentChanged([]int64{aliceHostID}, proto.AgentChangedEventData{
			ChangedLogs: []string{aliceLog},
		})
	}
	lead := rdAlice.next(t, 500*time.Millisecond)
	if lead.T != "convo" {
		t.Fatalf("expected leading convo push, got %+v", lead)
	}
	// Nothing else should arrive during the first 60ms of the 120ms window.
	rdAlice.expectNone(t, 60*time.Millisecond)
	// Trailing flush arrives after the 120ms window expires.
	trail := rdAlice.next(t, 500*time.Millisecond)
	if trail.T != "convo" {
		t.Fatalf("expected trailing convo push, got %+v", trail)
	}
	rdAlice.expectNone(t, 150*time.Millisecond)

	// 6. Verify /api/admin/perf records push hints, sent, coalesced, AND convo.read trigger split.
	mustStatus(t, alice.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?src=push", aliceSessID), nil), http.StatusOK, "convo push")
	mustStatus(t, alice.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?src=poll", aliceSessID), nil), http.StatusOK, "convo poll")
	mustStatus(t, alice.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?src=poll", aliceSessID), nil), http.StatusOK, "convo poll 2")

	perfResp := admin.do("GET", "/api/admin/perf", nil)
	mustStatus(t, perfResp, http.StatusOK, "admin perf")
	var snap perf.Snapshot
	if err := json.Unmarshal(perfResp.Body, &snap); err != nil {
		t.Fatalf("unmarshal perf: %v", err)
	}
	if snap.Ops["push.corrald_hint"].Count < 10 {
		t.Fatalf("expected push.corrald_hint >= 10, got %+v", snap.Ops["push.corrald_hint"])
	}
	if snap.Ops["push.sent"].Count < 6 {
		t.Fatalf("expected push.sent >= 6, got %+v", snap.Ops["push.sent"])
	}
	if snap.Ops["push.coalesced"].Count < 4 {
		t.Fatalf("expected push.coalesced >= 4, got %+v", snap.Ops["push.coalesced"])
	}
	cr := snap.Ops["convo.read"]
	if cr.ByBucket["push"].Count != 1 || cr.ByBucket["poll"].Count != 2 {
		t.Fatalf("expected convo.read by_bucket push=1 poll=2, got %+v", cr.ByBucket)
	}
	trig := snap.Ops["convo.read.trigger"]
	if trig.Outcomes["push"] != 1 || trig.Outcomes["poll"] != 2 {
		t.Fatalf("expected convo.read.trigger push=1 poll=2, got %+v", trig.Outcomes)
	}
}

func TestCorraldPushLatencyAndHotUpdateE2E(t *testing.T) {
	v1Dir, v2Dir := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-push-e2e.local"
	})
	admin := h.admin()
	srv, home, _ := startCountingCorraldSSH(t)
	agents := spawnE2EFakeAgents(t, home)

	hostID, sessID := createAndPinHost(t, admin, srv, "push-e2e-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	sess, err := h.srv.terms.Get(uid, sessID)
	if err != nil {
		t.Fatalf("session not found: %v", err)
	}
	sess.Persistence = "herdr"
	h.srv.herdrSocks.set(sessID, agents.herdrSock)

	// Enable real corrald on this host.
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")

	// Open browser push WebSocket and watch hostID.
	ws, _, err := dialEventsWS(t, h, admin, h.cfg.Origin())
	if err != nil {
		t.Fatalf("dial events ws: %v", err)
	}
	defer ws.CloseNow()

	initCover := readPushMsg(t, ws, 2*time.Second)
	if initCover.T != "cover" {
		t.Fatalf("expected initial cover message, got %+v", initCover)
	}
	sendWatchHosts(t, ws, []int64{hostID})

	// Wait until hostID enters push coverage (corrald agent.watch succeeded).
	waitCover := func(want []int64, desc string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			rem := time.Until(deadline)
			m := readPushMsg(t, ws, rem)
			if m.T == "cover" && slices.Equal(m.Hosts, want) {
				return
			}
		}
		t.Fatalf("timed out waiting for cover=%v (%s)", want, desc)
	}
	waitCover([]int64{hostID}, "initial watch subscribe")

	// Read conversation once so gateway locates and binds (sessID, claudePane) -> (claudePID, claudeLog).
	r0 := admin.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?pane=%s", sessID, agents.claudePane), nil)
	mustStatus(t, r0, http.StatusOK, "initial conversation read")
	j0 := r0.json()
	gen := int64(j0["gen"].(float64))
	rev := int(j0["rev"].(float64))

	// Give corrald inotify a moment to settle watches on the newly bound log.
	time.Sleep(100 * time.Millisecond)

	// Measure end-to-end latency across 7 real log writes:
	//   log write -> inotify -> corrald agent.changed -> SSH -> gateway -> WS push -> browser GET ?src=push
	const rounds = 7
	hintLatencies := make([]time.Duration, 0, rounds)
	fullLatencies := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		// Space writes beyond coalesce window (150ms) so every write measures a fresh leading push.
		if i > 0 {
			time.Sleep(180 * time.Millisecond)
		}
		marker := fmt.Sprintf("push-reply-turn-%d", i)
		line := fmt.Sprintf(`{"type":"assistant","uuid":"a-push-%d","timestamp":"2026-11-28T02:00:0%dZ","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n", i, i, marker)

		tWrite := time.Now()
		f, err := os.OpenFile(agents.claudeLog, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open claude log: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			_ = f.Close()
			t.Fatalf("append claude log: %v", err)
		}
		_ = f.Close()

		// Wait for {"t":"convo","session":sessID,"pane":claudePane} on WS.
		deadline := time.Now().Add(3 * time.Second)
		for {
			m := readPushMsg(t, ws, time.Until(deadline))
			if m.T == "convo" && m.Session == sessID && m.Pane == agents.claudePane {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: timed out waiting for convo push", i)
			}
		}
		hintDur := time.Since(tWrite)
		hintLatencies = append(hintLatencies, hintDur)

		// Browser immediately initiates GET /api/sessions/{id}/conversation?src=push.
		r := admin.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?pane=%s&gen=%d&rev=%d&src=push", sessID, agents.claudePane, gen, rev), nil)
		mustStatus(t, r, http.StatusOK, "push-triggered convo read")
		fullDur := time.Since(tWrite)
		fullLatencies = append(fullLatencies, fullDur)
		if !strings.Contains(string(r.Body), marker) {
			t.Fatalf("round %d: push-triggered convo read missing %q: %s", i, marker, r.Body)
		}
		j := r.json()
		gen = int64(j["gen"].(float64))
		rev = int(j["rev"].(float64))
	}

	sort.Slice(hintLatencies, func(i, j int) bool { return hintLatencies[i] < hintLatencies[j] })
	sort.Slice(fullLatencies, func(i, j int) bool { return fullLatencies[i] < fullLatencies[j] })
	p50Hint := hintLatencies[len(hintLatencies)/2]
	maxHint := hintLatencies[len(hintLatencies)-1]
	p50Full := fullLatencies[len(fullLatencies)/2]
	maxFull := fullLatencies[len(fullLatencies)-1]
	t.Logf("E2E real corrald push latency (%d rounds): log->WS hint p50=%v max=%v; log->GET response p50=%v max=%v",
		rounds, p50Hint, maxHint, p50Full, maxFull)
	if p50Hint >= 300*time.Millisecond {
		t.Fatalf("log->WS hint p50=%v exceeded 300ms target", p50Hint)
	}

	// Verify health degradation immediately removes hostID from WS cover.
	h.srv.CorraldManager().SetHealthCapsForTest(hostID, map[string]string{"locate": "ok", "tails": "ok", "watch": "degraded:poll_fallback(limit=256)"})
	waitCover([]int64{}, "watch capability degraded")

	// Restore health -> hostID returns to cover.
	h.srv.CorraldManager().SetHealthCapsForTest(hostID, map[string]string{"locate": "ok", "tails": "ok", "watch": "ok"})
	waitCover([]int64{hostID}, "watch capability restored")

	// Hot-update corrald v1 -> v2 while browser is watching: verify auto re-subscribe works on v2.
	dist2, err := gw.LoadDist(v2Dir)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.CorraldManager().SetDist(dist2)
	hk, err := h.store.HostKey(context.Background(), uid, hostID)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.CorraldManager().OnHostConnected(context.Background(), uid, hostID, "admin", hk.Fingerprint, "alice", sess.Client())

	// Wait for v2 handoff to complete.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := admin.do("GET", fmt.Sprintf("/api/hosts/%d/corrald", hostID), nil).json()
		if st["state"] == "running" && int64(st["release"].(float64)) == dist2.Release {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !h.srv.CorraldManager().IsWatchCovered(uid, hostID) {
		t.Fatal("expected hostID to remain watch-covered after v1->v2 hot update")
	}

	// Append another log line after hot update -> must STILL push convo hint!
	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(agents.claudeLog, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","uuid":"a-after-v2","timestamp":"2026-11-28T02:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"after-v2-handoff"}]}}` + "\n")
	_ = f.Close()

	afterDeadline := time.Now().Add(3 * time.Second)
	for {
		m := readPushMsg(t, ws, time.Until(afterDeadline))
		if m.T == "convo" && m.Session == sessID && m.Pane == agents.claudePane {
			break
		}
		if time.Now().After(afterDeadline) {
			t.Fatal("timed out waiting for convo push after v1->v2 handoff")
		}
	}
}

// Regression A.5: herdr summary changes (NotifyAgentsChanged) must NOT push
// agents hints when corrald is not covering the host, leaving behavior
// identical when corrald is off.
func TestHerdrNotifyAgentsOnlyWhenHostCovered(t *testing.T) {
	v1Dir, _ := buildTestCorraldDist(t)
	h := newHarness(t, func(c *config.Config) {
		c.CorraldDir = v1Dir
		c.GatewayID = "gw-herdr-gate.local"
	})
	admin := h.admin()
	srv, _ := startCorraldSSH(t)
	hostID, _ := createAndPinHost(t, admin, srv, "herdr-gate-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))

	ws, _, err := dialEventsWS(t, h, admin, h.cfg.Origin())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	rd := startPushReader(ws)
	_ = rd.next(t, 2*time.Second) // initial cover=[]
	sendWatchHosts(t, ws, []int64{hostID})
	time.Sleep(40 * time.Millisecond)

	// Corrald is OFF (host not in cover): NotifyAgentsChanged must NOT push anything.
	h.srv.events.NotifyAgentsChanged(uid, hostID)
	rd.expectNone(t, 180*time.Millisecond)

	// Enable corrald and wait until hostID enters cover.
	startCorraldInstall(t, admin, hostID)
	waitCorraldState(t, admin, hostID, "running")
	deadline := time.Now().Add(5 * time.Second)
	for {
		m := rd.next(t, time.Until(deadline))
		if m.T == "cover" && slices.Equal(m.Hosts, []int64{hostID}) {
			break
		}
	}

	// Now that hostID IS in cover, NotifyAgentsChanged pushes {"t":"agents","host":hostID}.
	h.srv.events.NotifyAgentsChanged(uid, hostID)
	got := rd.next(t, 2*time.Second)
	if got.T != "agents" || got.Host != hostID {
		t.Fatalf("expected agents(%d) when covered, got %+v", hostID, got)
	}
}

// Regression B: When an unmatched log file changes (new conversation's first
// reply or mid-conversation /clear / /resume), bindings with no log or a quiet
// log (>= 2s) receive a convo hint and relocAfter mark, while another actively
// streaming conversation on the same host is NOT disturbed.
func TestEventsUnmatchedLogWakesNoLogAndQuietBindingsNotActive(t *testing.T) {
	h := newHarness(t)
	h.srv.events.SetCoalesceWindow(80 * time.Millisecond)
	h.srv.events.SetUnmatchedLogQuietThreshold(2 * time.Second)

	admin := h.admin()
	alice := h.member(admin, "alice")
	srv := h.sshServer(t.TempDir())
	hostID, sessID := createAndPinHost(t, alice, srv, "newlog-box")
	aliceID := int64(alice.do("GET", "/api/me", nil).json()["id"].(float64))

	// Create convoStates for three panes on the same session/host so we can
	// verify relocAfter is only set for the no-log and quiet panes.
	stNoLog := h.srv.convos.get(sessID + "\x00w1:p_nolog")
	stNoLog.done()
	stQuiet := h.srv.convos.get(sessID + "\x00w1:p_quiet")
	stQuiet.done()
	stActive := h.srv.convos.get(sessID + "\x00w1:p_active")
	stActive.done()

	// 1) No-log binding (brand new Claude Code conversation before first reply).
	h.srv.events.RecordBindingWithActivity(aliceID, hostID, sessID, "w1:p_nolog", "w1:p_nolog", 50001, "", time.Time{})
	// 2) Quiet binding (last log change 5s ago, e.g. user ran /clear or /resume).
	h.srv.events.RecordBindingWithActivity(aliceID, hostID, sessID, "w1:p_quiet", "w1:p_quiet", 50002, "/tmp/old-quiet.jsonl", time.Now().Add(-5*time.Second))
	// 3) Actively streaming binding (last log change right now).
	h.srv.events.RecordBindingWithActivity(aliceID, hostID, sessID, "w1:p_active", "w1:p_active", 50003, "/tmp/streaming-now.jsonl", time.Now())

	ws, _, err := dialEventsWS(t, h, alice, h.cfg.Origin())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	rd := startPushReader(ws)
	_ = rd.next(t, 2*time.Second) // initial cover
	sendWatchHosts(t, ws, []int64{hostID})
	time.Sleep(40 * time.Millisecond)

	// Emit an unmatched log change (a brand new log path appearing on the host).
	h.srv.events.HandleAgentChanged([]int64{hostID}, proto.AgentChangedEventData{
		ChangedLogs: []string{"/tmp/brand-new-first-reply.jsonl"},
	})

	// Expect exactly 3 messages: convo(w1:p_nolog), convo(w1:p_quiet), and agents(hostID).
	// w1:p_active MUST NOT receive a convo hint!
	gotConvo := map[string]bool{}
	gotAgents := false
	for i := 0; i < 3; i++ {
		m := rd.next(t, 2*time.Second)
		switch m.T {
		case "convo":
			gotConvo[m.Pane] = true
		case "agents":
			if m.Host == hostID {
				gotAgents = true
			}
		}
	}
	rd.expectNone(t, 160*time.Millisecond)

	if !gotConvo["w1:p_nolog"] {
		t.Fatalf("no-log binding did not receive convo hint: %+v", gotConvo)
	}
	if !gotConvo["w1:p_quiet"] {
		t.Fatalf("quiet binding did not receive convo hint: %+v", gotConvo)
	}
	if gotConvo["w1:p_active"] {
		t.Fatalf("actively streaming binding should NOT receive convo hint from unrelated log: %+v", gotConvo)
	}
	if !gotAgents {
		t.Fatal("expected agents hint alongside unmatched log")
	}
	if stNoLog.relocAfter.Load() == 0 {
		t.Fatal("expected relocAfter set on no-log convoState so next GET immediately locates new log")
	}
	if stQuiet.relocAfter.Load() == 0 {
		t.Fatal("expected relocAfter set on quiet convoState so next GET immediately locates switched log")
	}
	if stActive.relocAfter.Load() != 0 {
		t.Fatal("active convoState must NOT have relocAfter forced by unrelated unmatched log")
	}
}

// Regression fix2 #2: When corrald emits ONLY added_pids (no changed_logs) —
// e.g. a Claude Code process creates its first log or switches to a new log on
// the same PID, or a new agent PID starts in a pane — the gateway must set
// relocAfter alongside the convo hint so the browser's immediate src=push GET
// relocates and reads the new log immediately instead of waiting for the 10s
// locate TTL.
func TestEventsAddedPIDsTriggersRelocateAndReadsNewLog(t *testing.T) {
	h := newHarness(t)
	h.srv.events.SetCoalesceWindow(60 * time.Millisecond)
	h.srv.events.SetUnmatchedLogQuietThreshold(2 * time.Second)

	admin := h.admin()
	srv, home, _ := startCountingCorraldSSH(t)
	agents := spawnE2EFakeAgents(t, home)

	// Start with NO log file for Claude (brand-new conversation before first reply).
	if err := os.Remove(agents.claudeLog); err != nil {
		t.Fatalf("remove initial claude log: %v", err)
	}

	hostID, sessID := createAndPinHost(t, admin, srv, "added-pids-box")
	uid := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	sess, err := h.srv.terms.Get(uid, sessID)
	if err != nil {
		t.Fatalf("session not found: %v", err)
	}
	sess.Persistence = "herdr"
	h.srv.herdrSocks.set(sessID, agents.herdrSock)

	ws, _, err := dialEventsWS(t, h, admin, h.cfg.Origin())
	if err != nil {
		t.Fatalf("dial events ws: %v", err)
	}
	defer ws.CloseNow()
	rd := startPushReader(ws)
	_ = rd.next(t, 2*time.Second) // initial cover
	sendWatchHosts(t, ws, []int64{hostID})
	time.Sleep(40 * time.Millisecond)

	// 1. Initial GET locates the Claude PID with no log yet -> returns reason="no_log"
	//    and caches st.located=now (well within 10s convoLocateTTL).
	r0 := admin.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?pane=%s", sessID, agents.claudePane), nil)
	mustStatus(t, r0, http.StatusOK, "initial no-log conversation read")
	j0 := r0.json()
	if j0["reason"] != convoNoLog {
		t.Fatalf("expected initial reason=%q, got %+v", convoNoLog, j0)
	}

	// Write the first reply log file on disk, then deliver ONLY added_pids (no changed_logs).
	firstReply := "added-pids-first-reply-visible-immediately"
	firstBody := strings.Join([]string{
		`{"type":"user","uuid":"u-ap-1","timestamp":"2026-11-28T04:00:00Z","message":{"role":"user","content":"hello"}}`,
		fmt.Sprintf(`{"type":"assistant","uuid":"a-ap-1","timestamp":"2026-11-28T04:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, firstReply),
		"",
	}, "\n")
	if err := os.WriteFile(agents.claudeLog, []byte(firstBody), 0o600); err != nil {
		t.Fatalf("write claude first log: %v", err)
	}

	h.srv.events.HandleAgentChanged([]int64{hostID}, proto.AgentChangedEventData{
		AddedPIDs: []int{agents.claudePID},
	})

	// Expect convo hint for claudePane.
	deadline := time.Now().Add(2 * time.Second)
	for {
		m := rd.next(t, time.Until(deadline))
		if m.T == "convo" && m.Session == sessID && m.Pane == agents.claudePane {
			break
		}
	}

	// Immediate src=push GET (still well inside 10s locateTTL) MUST relocate and read the new log!
	r1 := admin.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?pane=%s&src=push", sessID, agents.claudePane), nil)
	mustStatus(t, r1, http.StatusOK, "src=push read after added_pids (first log)")
	if !strings.Contains(string(r1.Body), firstReply) {
		t.Fatalf("src=push read after added_pids did not relocate to new log: %s", r1.Body)
	}
	j1 := r1.json()
	gen1 := int64(j1["gen"].(float64))
	rev1 := int(j1["rev"].(float64))

	// 2. Simulate same PID switching to a brand-new log (/clear or /resume) with ONLY added_pids.
	time.Sleep(100 * time.Millisecond) // clear coalesce window
	newSID := "22222222-2222-4222-8222-222222222222"
	switchedReply := "added-pids-switched-log-content"
	newLogPath := filepath.Join(filepath.Dir(agents.claudeLog), newSID+".jsonl")
	switchedBody := strings.Join([]string{
		`{"type":"user","uuid":"u-ap-2","timestamp":"2026-11-28T04:01:00Z","message":{"role":"user","content":"fresh"}}`,
		fmt.Sprintf(`{"type":"assistant","uuid":"a-ap-2","timestamp":"2026-11-28T04:01:01Z","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, switchedReply),
		"",
	}, "\n")
	if err := os.WriteFile(newLogPath, []byte(switchedBody), 0o600); err != nil {
		t.Fatalf("write switched claude log: %v", err)
	}
	sessFile := filepath.Join(home, ".claude", "sessions", strconv.Itoa(agents.claudePID)+".json")
	origSessRaw, err := os.ReadFile(sessFile)
	if err != nil {
		t.Fatalf("read claude session file: %v", err)
	}
	var sessDoc map[string]any
	if err := json.Unmarshal(origSessRaw, &sessDoc); err != nil {
		t.Fatalf("unmarshal session file: %v", err)
	}
	sessDoc["sessionId"] = newSID
	updatedSessRaw, err := json.Marshal(sessDoc)
	if err != nil {
		t.Fatalf("marshal updated session file: %v", err)
	}
	if err := os.WriteFile(sessFile, updatedSessRaw, 0o600); err != nil {
		t.Fatalf("write updated session file: %v", err)
	}

	h.srv.events.HandleAgentChanged([]int64{hostID}, proto.AgentChangedEventData{
		AddedPIDs: []int{agents.claudePID},
	})
	deadline = time.Now().Add(2 * time.Second)
	for {
		m := rd.next(t, time.Until(deadline))
		if m.T == "convo" && m.Session == sessID && m.Pane == agents.claudePane {
			break
		}
	}

	r2 := admin.do("GET", fmt.Sprintf("/api/sessions/%s/conversation?pane=%s&gen=%d&rev=%d&src=push", sessID, agents.claudePane, gen1, rev1), nil)
	mustStatus(t, r2, http.StatusOK, "src=push read after added_pids (switched log)")
	if !strings.Contains(string(r2.Body), switchedReply) {
		t.Fatalf("src=push read after same-PID log switch did not relocate to new log: %s", r2.Body)
	}

	// 3. Unmatched added_pids wakes no-log and quiet (>=2s) bindings with relocAfter,
	//    while leaving an actively streaming binding untouched.
	time.Sleep(100 * time.Millisecond)
	stNoLog := h.srv.convos.get(sessID + "\x00w2:p_nolog")
	stNoLog.done()
	stQuiet := h.srv.convos.get(sessID + "\x00w2:p_quiet")
	stQuiet.done()
	stActive := h.srv.convos.get(sessID + "\x00w2:p_active")
	stActive.done()

	h.srv.events.RecordBindingWithActivity(uid, hostID, sessID, "w2:p_nolog", "w2:p_nolog", 60001, "", time.Time{})
	h.srv.events.RecordBindingWithActivity(uid, hostID, sessID, "w2:p_quiet", "w2:p_quiet", 60002, "/tmp/quiet-ap.jsonl", time.Now().Add(-5*time.Second))
	h.srv.events.RecordBindingWithActivity(uid, hostID, sessID, "w2:p_active", "w2:p_active", 60003, "/tmp/active-ap.jsonl", time.Now())

	// Drain any earlier queued hints before testing unmatched added_pids.
	for {
		select {
		case <-rd.ch:
		default:
			goto drained
		}
	}
drained:
	h.srv.events.HandleAgentChanged([]int64{hostID}, proto.AgentChangedEventData{
		AddedPIDs: []int{agents.claudePID + 99999},
	})

	gotPanes := map[string]bool{}
	waitEnd := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(waitEnd) {
		select {
		case m := <-rd.ch:
			if m.T == "convo" {
				gotPanes[m.Pane] = true
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !gotPanes["w2:p_nolog"] || stNoLog.relocAfter.Load() == 0 {
		t.Fatalf("no-log pane must receive convo + relocAfter on unmatched added_pids (got=%+v reloc=%d)", gotPanes, stNoLog.relocAfter.Load())
	}
	if !gotPanes["w2:p_quiet"] || stQuiet.relocAfter.Load() == 0 {
		t.Fatalf("quiet pane must receive convo + relocAfter on unmatched added_pids (got=%+v reloc=%d)", gotPanes, stQuiet.relocAfter.Load())
	}
	if gotPanes["w2:p_active"] || stActive.relocAfter.Load() != 0 {
		t.Fatalf("active pane must NOT receive convo or relocAfter on unrelated added_pids (got=%+v reloc=%d)", gotPanes, stActive.relocAfter.Load())
	}
}
