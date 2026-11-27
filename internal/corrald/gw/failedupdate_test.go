package gw

import (
	"context"
	"errors"
	"testing"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
)

// A release marked failed on the host (a crash rollback, a failed handoff)
// is never started again by an update, even one a user confirms from a page
// that still shows the old prompt.
func TestStartUpdateRefusesFailedRelease(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dbFailed    []int64
		connFailed  []int64
		wantRefused bool
	}{
		{"failed in db", []int64{2000}, nil, true},
		{"failed on the live connection only", nil, []int64{2000}, true},
		{"other release failed", []int64{1500}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			bg := context.Background()
			u, _ := st.CreateUser(bg, "alice", "Alice", "hash", "admin")
			h, _ := st.CreateHost(bg, &store.Host{OwnerID: u.ID, Name: "box", Address: "127.0.0.1", Port: 22, Username: "alice", Auth: "password"})
			m := proto.DefaultManifest()
			mj, _ := proto.MarshalManifest(m)
			hash, _ := proto.ManifestSHA256(m)
			if err := st.UpsertCorraldHost(bg, &store.CorraldHost{
				HostID: h.ID, State: StateRunning, BoundFingerprint: "fp-1", BoundUsername: "alice", BoundGateway: "gw-1",
				ManifestJSON: string(mj), Version: "0.3.1", Release: 1000, FailedReleases: tc.dbFailed,
			}); err != nil {
				t.Fatal(err)
			}
			mgr := NewManager(ManagerConfig{
				Store:     st,
				Dist:      &Dist{Version: "0.3.2", Release: 2000, Manifest: m, ManifestHash: hash},
				GatewayID: "gw-1",
				SSH:       &stubSSHProvider{bindings: map[int64][2]string{h.ID: {"alice", "fp-1"}}},
			})
			t.Cleanup(mgr.Close)
			if tc.connFailed != nil {
				key := accountKey{Fingerprint: "fp-1", Username: "alice"}
				sc := newSharedConn(key)
				sc.failedReleases = tc.connFailed
				sc.hello = &proto.HelloResult{Version: "0.3.1", Release: 1000, Manifest: m}
				mgr.mu.Lock()
				mgr.conns[key] = sc
				mgr.hostConn[h.ID] = key
				mgr.mu.Unlock()
			}
			_, err = mgr.StartUpdate(bg, u.ID, h.ID, "alice", hash, false)
			refused := errors.Is(err, ErrInvalidState)
			if refused != tc.wantRefused {
				t.Fatalf("StartUpdate err = %v, refused=%v want %v", err, refused, tc.wantRefused)
			}
			if tc.wantRefused {
				if op, oerr := st.ActiveCorraldOp(bg, h.ID); oerr == nil && op != nil {
					t.Fatalf("a refused update left an op: %+v", op)
				}
			}
		})
	}
}
