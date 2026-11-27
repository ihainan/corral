package gw

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

func TestLoadDistMissingAndValid(t *testing.T) {
	if _, err := LoadDist(t.TempDir()); !errors.Is(err, ErrDistUnavailable) {
		t.Fatalf("empty dir: got %v, want ErrDistUnavailable", err)
	}

	dir := t.TempDir()
	for _, arch := range SupportedArches {
		p := filepath.Join(dir, "linux-"+arch, "corrald")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("binary-"+arch), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mb, _ := proto.MarshalManifest(proto.DefaultManifest())
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte(`{"version":"0.4.0","release":1700000000,"commit":"abc123","protocol":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	dist, err := LoadDist(dir)
	if err != nil {
		t.Fatalf("LoadDist valid dir: %v", err)
	}
	if dist.Version != "0.4.0" || dist.Release != 1700000000 || dist.Commit != "abc123" {
		t.Fatalf("unexpected metadata: %+v", dist)
	}
	if dist.ManifestHash == "" {
		t.Fatal("expected non-empty ManifestHash")
	}
	if b, ok := dist.Binary("amd64"); !ok || len(b.SHA256) != 64 {
		t.Fatalf("unexpected amd64 binary: %+v %v", b, ok)
	}
	if b, ok := dist.Binary("arm64"); !ok || len(b.SHA256) != 64 {
		t.Fatalf("unexpected arm64 binary: %+v %v", b, ok)
	}
}

func TestClientCallEventAndUnknownWriteResult(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	evCh := make(chan proto.Event, 4)
	client := NewClient(a, func(ev proto.Event) { evCh <- ev })
	defer client.Close()

	// Fake server on b.
	go func() {
		for {
			var req proto.Request
			if err := proto.ReadJSON(b, &req); err != nil {
				return
			}
			switch req.Method {
			case proto.MethodDaemonHello:
				res, _ := json.Marshal(proto.HelloResult{
					Version:  "0.3.1",
					Release:  100,
					Protocol: 1,
					Epoch:    "ep-1",
					Seq:      5,
					PID:      42,
					State:    proto.StateRunning,
				})
				_ = proto.WriteJSON(b, proto.Response{ID: req.ID, Result: res})
				_ = proto.WriteJSON(b, proto.Event{Event: proto.EventAgentStatus, Epoch: "ep-1", Seq: 6})
			case proto.MethodPaneSend:
				// Drop connection mid-write without replying!
				_ = b.Close()
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	hello, err := client.Hello(ctx, proto.HelloParams{Gateway: "gw", ProtoMin: 1, ProtoMax: 1})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if hello.Version != "0.3.1" || hello.PID != 42 {
		t.Fatalf("unexpected hello: %+v", hello)
	}

	select {
	case ev := <-evCh:
		if ev.Event != proto.EventAgentStatus || ev.Seq != 6 {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}

	ep, seq := client.Cursor()
	if ep != "ep-1" || seq != 6 {
		t.Fatalf("Cursor = (%q, %d), want (ep-1, 6)", ep, seq)
	}

	// Mutating call disconnected in flight MUST return ErrUnknownResult.
	err = client.CallMutating(ctx, proto.MethodPaneSend, map[string]string{"op_id": "op1"}, nil)
	if !errors.Is(err, ErrUnknownResult) {
		t.Fatalf("CallMutating after disconnect = %v, want ErrUnknownResult", err)
	}
}

func TestParseProbeAndSocketFallback(t *testing.T) {
	raw := []byte("os=Linux\nmachine=x86_64\nuid=1000\nhome=/home/dev\nfree_kb=500000\nmid_raw=0123456789abcdef0123456789abcdef\nmid_src=/etc/machine-id\nhome_writable=1\nhas_symlink=1\ncurrent_target=corrald-0.3.1-100/corrald\nversion_dir=corrald-0.3.1-100\nresult=ok\n")
	p, err := ParseProbe(raw)
	if err != nil {
		t.Fatalf("ParseProbe: %v", err)
	}
	if p.OS != "Linux" || p.Arch != "amd64" || p.MachineID != "0123456789ab" || !p.HomeWritable || !p.HasSymlink {
		t.Fatalf("unexpected probe: %+v", p)
	}
	if p.SocketPath != "/home/dev/.local/state/corral/corrald/0123456789ab/corrald.sock" {
		t.Fatalf("unexpected socket path: %s", p.SocketPath)
	}

	longHome := "/" + strings.Repeat("a", 80)
	longSock := ComputeSocketPath(longHome, "1000", "0123456789ab")
	if !strings.HasPrefix(longSock, "/tmp/corrald-1000-") || len(longSock) > MaxSocketPath {
		t.Fatalf("expected /tmp fallback socket <= %d bytes, got %q (%d)", MaxSocketPath, longSock, len(longSock))
	}

	left := ParseLeftLines([]byte("result=partial\nleft=/tmp/a\tpermission denied\nleft=/tmp/b\tbusy\n"))
	if len(left) != 2 || left[0].Path != "/tmp/a" || left[0].Reason != "permission denied" {
		t.Fatalf("unexpected left items: %+v", left)
	}
}
