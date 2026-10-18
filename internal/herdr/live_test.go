package herdr

// Read-only check against a real host. Skipped unless CORRAL_HERDR_LIVE is set,
// e.g. CORRAL_HERDR_LIVE=user@host CORRAL_HERDR_KEY=~/.ssh/id_ed25519.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestLiveHerdrReadOnly(t *testing.T) {
	target := os.Getenv("CORRAL_HERDR_LIVE")
	if target == "" {
		t.Skip("set CORRAL_HERDR_LIVE=user@host to run")
	}
	user, host, _ := strings.Cut(target, "@")
	keyPath := os.Getenv("CORRAL_HERDR_KEY")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ssh.Dial("tcp", host+":22", &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessions, err := Discover(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sessions: %+v", sessions)
	var sock string
	for _, s := range sessions {
		if s.Default && s.Running {
			sock = s.SocketPath
		}
	}
	if sock == "" {
		t.Skip("no running default session")
	}
	cl, err := Dial(c, sock)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cl.Snapshot(ctx)
	cl.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("version %s, %d workspaces, %d panes, %d agents", snap.Version, len(snap.Workspaces), len(snap.Panes), len(snap.Agents))
	for _, a := range snap.Agents {
		t.Logf("  agent %s %s %q", a.Agent, a.AgentStatus, a.Title)
	}
	// The event stream must open; we only listen for a few seconds.
	ev, err := Dial(c, sock)
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithTimeout(ctx, 4*time.Second)
	defer scancel()
	n := 0
	subs := []map[string]any{{"type": "workspace.updated"}, {"type": "pane.created"}}
	for _, p := range snap.Panes {
		subs = append(subs, map[string]any{"type": "pane.agent_status_changed", "pane_id": p.ID})
	}
	err = ev.Subscribe(sctx, subs, func(m json.RawMessage) { n++ })
	t.Logf("subscription ended after %d events: %v", n, err)
	if err != nil && sctx.Err() == nil {
		t.Fatalf("subscription failed: %v", err)
	}
}
