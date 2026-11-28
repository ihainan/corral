package gw

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
)

func openTestStoreForGW(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	u, err := st.CreateUser(context.Background(), "tester", "Tester", "hash", "admin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	h, err := st.CreateHost(context.Background(), &store.Host{
		OwnerID:     u.ID,
		Name:        "box1",
		Address:     "127.0.0.1",
		Port:        22,
		Username:    "dev",
		Auth:        "agent",
		Persistence: "tmux",
	})
	if err != nil {
		t.Fatalf("CreateHost: %v", err)
	}
	return st, u.ID, h.ID
}

// Regression test for Review Item #2 [P1]:
// When client.Health fails during SetAdminDisabled(true), enforceAdminDisableOnConn
// MUST NOT fall back to cached sc.health.ConnectedGateways==1 to stop the daemon;
// it must treat the failure as "other gateway present" (no Shutdown, DB marked stopped).
func TestEnforceAdminDisableHealthFailureNeverUsesCache(t *testing.T) {
	st, _, hostID := openTestStoreForGW(t)
	ctx := context.Background()

	fp := "SHA256:test-fp-health-fail"
	sshUser := "dev"
	gwID := "gw-self.local"

	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           hostID,
		State:            StateRunning,
		BoundFingerprint: fp,
		BoundUsername:    sshUser,
		BoundGateway:     gwID,
		Version:          "0.3.1",
		Release:          1000,
	}); err != nil {
		t.Fatalf("UpsertCorraldHost: %v", err)
	}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var healthCalls atomic.Int32
	var shutdownCalls atomic.Int32
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			b, err := proto.ReadFrame(serverConn)
			if err != nil {
				return
			}
			var req proto.Request
			if err := json.Unmarshal(b, &req); err != nil {
				return
			}
			switch req.Method {
			case proto.MethodDaemonHealth:
				healthCalls.Add(1)
				resp := proto.Response{
					ID: req.ID,
					Error: &proto.RPCError{
						Code:    "internal",
						Message: "health probe failed",
					},
				}
				rb, _ := json.Marshal(resp)
				_ = proto.WriteFrame(serverConn, rb)
			case proto.MethodDaemonShutdown:
				shutdownCalls.Add(1)
				res, _ := json.Marshal(proto.ShutdownResult{})
				resp := proto.Response{ID: req.ID, Result: res}
				rb, _ := json.Marshal(resp)
				_ = proto.WriteFrame(serverConn, rb)
			}
		}
	}()

	cl := NewClient(clientConn, nil)
	defer cl.Close()

	m := NewManager(ManagerConfig{
		Store:     st,
		GatewayID: gwID,
	})
	defer m.Close()

	key := accountKey{Fingerprint: fp, Username: sshUser}
	sc := newSharedConn(key)
	sc.client = cl
	sc.hello = &proto.HelloResult{
		Version: "0.3.1",
		Release: 1000,
		Clients: []proto.ClientInfo{{Gateway: gwID, CorralUser: "tester"}},
	}
	// Cached health says only 1 gateway connected! The old implementation fell
	// back to this cached value when live Health failed and called Shutdown.
	sc.health = &proto.HealthResult{
		PID:               4242,
		ConnectedGateways: 1,
	}
	sc.hostRefs[hostID] = &hostRef{HostID: hostID, OwnerID: 1, CorralUser: "tester"}

	m.mu.Lock()
	m.conns[key] = sc
	m.hostConn[hostID] = key
	m.mu.Unlock()

	m.SetAdminDisabled(ctx, true)

	if healthCalls.Load() == 0 {
		t.Fatalf("expected live daemon.health RPC to be attempted")
	}
	if shutdownCalls.Load() != 0 {
		t.Fatalf("must NOT call daemon.shutdown when live Health fails (got %d calls)", shutdownCalls.Load())
	}

	ch, err := st.CorraldHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("CorraldHostByID: %v", err)
	}
	if ch.State != StateStopped {
		t.Fatalf("expected host DB state %q when Health fails, got %q", StateStopped, ch.State)
	}
	if !cl.Closed() {
		t.Fatalf("expected gateway client connection to be closed")
	}
}

func TestHostClientsFileUnmarshalFormats(t *testing.T) {
	// Actual format written by daemon.ClientRegistry.saveLocked: JSON array.
	var cfArr HostClientsFile
	if err := json.Unmarshal([]byte(`[{"gateway":"gw-a","corral_user":"alice"},{"gateway":"gw-b","corral_user":"bob"}]`), &cfArr); err != nil {
		t.Fatalf("unmarshal array clients.json: %v", err)
	}
	if len(cfArr.Clients) != 2 || cfArr.Clients[0].Gateway != "gw-a" || cfArr.Clients[1].Gateway != "gw-b" {
		t.Fatalf("unexpected array clients: %+v", cfArr.Clients)
	}

	// Legacy wrapped object format.
	var cfObj HostClientsFile
	if err := json.Unmarshal([]byte(`{"clients":[{"gateway":"gw-a","corral_user":"alice"}]}`), &cfObj); err != nil {
		t.Fatalf("unmarshal object clients.json: %v", err)
	}
	if len(cfObj.Clients) != 1 || cfObj.Clients[0].Gateway != "gw-a" {
		t.Fatalf("unexpected object clients: %+v", cfObj.Clients)
	}
}

func TestPerfCompareDateBoundsExcludesConsentDay(t *testing.T) {
	consent := time.Date(2026, 11, 26, 15, 30, 0, 0, time.UTC).Unix()
	beforeMin, beforeMax, afterMin := PerfCompareDateBounds(consent)
	if beforeMin != "2026-10-27" || beforeMax != "2026-11-25" || afterMin != "2026-11-27" {
		t.Fatalf("unexpected bounds: beforeMin=%q beforeMax=%q afterMin=%q", beforeMin, beforeMax, afterMin)
	}
}
