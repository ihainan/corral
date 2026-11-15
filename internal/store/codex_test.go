package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCodexCommandsOwnershipAndConcurrentAcceptance(t *testing.T) {
	e := newTabEnv(t)
	var fresh atomic.Int32
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			previous, created, err := e.s.BeginCodexCommand(e.ctx, e.user.ID, e.host.ID, "key", "digest")
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				fresh.Add(1)
			}
			if previous.Status != 409 || previous.Digest != "digest" {
				t.Errorf("invalid initial record: %+v", previous)
			}
		}()
	}
	group.Wait()
	if fresh.Load() != 1 {
		t.Fatalf("accepted %d times", fresh.Load())
	}
	if _, _, err := e.s.BeginCodexCommand(e.ctx, e.user.ID+100, e.host.ID, "other", "digest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign host: %v", err)
	}
	if err := e.s.FinishCodexCommand(e.ctx, e.user.ID+100, e.host.ID, "key", 200, `{}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign result write: %v", err)
	}
	if err := e.s.FinishCodexCommand(e.ctx, e.user.ID, e.host.ID, "key", 200, `{"accepted":true}`); err != nil {
		t.Fatal(err)
	}
	got, created, err := e.s.BeginCodexCommand(e.ctx, e.user.ID, e.host.ID, "key", "new-digest")
	if err != nil || created || got.Digest != "digest" || got.Status != 200 {
		t.Fatalf("duplicate changed record: %+v %v %v", got, created, err)
	}
}

func TestCodexCommandsUnknownSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "alice", "", "x", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.CreateHost(ctx, &Host{OwnerID: u.ID, Name: "h", Address: "127.0.0.1", Port: 22, Username: "a", Auth: AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.BeginCodexCommand(ctx, u.ID, h.ID, "lost-response", "digest"); err != nil || !fresh {
		t.Fatalf("begin: %v %v", fresh, err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, fresh, err := s.BeginCodexCommand(ctx, u.ID, h.ID, "lost-response", "digest")
	if err != nil || fresh || got.Status != 409 || got.Result != `{"error":"outcome_unknown"}` {
		t.Fatalf("restart allowed resend: %+v %v %v", got, fresh, err)
	}
}
