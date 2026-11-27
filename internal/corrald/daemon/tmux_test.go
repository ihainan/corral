//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// Review #15: ValidateTmuxBinary checks basename, executable regular file,
// owner (root or current uid), and non-world-writable parent directory;
// `corrald serve --tmux` validates and records tmux_path in meta.json.
func TestValidateTmuxBinaryAndServeTmuxFlag(t *testing.T) {
	uid := uint32(os.Geteuid())

	// Safe custom directory (0755) with executable regular file named "tmux".
	safeDir := filepath.Join(t.TempDir(), "custom-bin")
	if err := os.MkdirAll(safeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	validTmux := filepath.Join(safeDir, "tmux")
	if err := os.WriteFile(validTmux, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Valid symlink (e.g. Nix profile) pointing to validTmux.
	symDir := filepath.Join(t.TempDir(), "nix-profile", "bin")
	if err := os.MkdirAll(symDir, 0o755); err != nil {
		t.Fatal(err)
	}
	validSym := filepath.Join(symDir, "tmux")
	if err := os.Symlink(validTmux, validSym); err != nil {
		t.Fatal(err)
	}

	got, err := ValidateTmuxBinary(validTmux, uid)
	if err != nil || got != validTmux {
		t.Fatalf("ValidateTmuxBinary(valid) = (%q, %v), want (%q, nil)", got, err, validTmux)
	}
	gotSym, err := ValidateTmuxBinary(validSym, uid)
	if err != nil || gotSym != validTmux {
		t.Fatalf("ValidateTmuxBinary(validSym) = (%q, %v), want (%q, nil)", gotSym, err, validTmux)
	}

	// Bad cases that must all be rejected:
	// 1. Relative path
	if _, err := ValidateTmuxBinary("tmux", uid); err == nil {
		t.Error("accepted relative tmux path")
	}
	// 2. Wrong basename
	if _, err := ValidateTmuxBinary("/bin/sh", uid); err == nil {
		t.Error("accepted /bin/sh")
	}
	// 3. Symlink named "tmux" pointing to non-tmux binary (/bin/sh)
	evilSym := filepath.Join(t.TempDir(), "tmux")
	_ = os.Symlink("/bin/sh", evilSym)
	if _, err := ValidateTmuxBinary(evilSym, uid); err == nil {
		t.Error("accepted symlink named tmux pointing to /bin/sh")
	}
	// 4. World-writable parent directory
	wwDir := filepath.Join(t.TempDir(), "ww-bin")
	_ = os.MkdirAll(wwDir, 0o777)
	_ = os.Chmod(wwDir, 0o777)
	wwTmux := filepath.Join(wwDir, "tmux")
	_ = os.WriteFile(wwTmux, []byte("#!/bin/sh\n"), 0o755)
	if _, err := ValidateTmuxBinary(wwTmux, uid); err == nil {
		t.Error("accepted tmux inside world-writable directory")
	}
	// 5. World-writable binary
	wwBinDir := filepath.Join(t.TempDir(), "ww-file-bin")
	_ = os.MkdirAll(wwBinDir, 0o755)
	wwBin := filepath.Join(wwBinDir, "tmux")
	_ = os.WriteFile(wwBin, []byte("#!/bin/sh\n"), 0o777)
	_ = os.Chmod(wwBin, 0o777)
	if _, err := ValidateTmuxBinary(wwBin, uid); err == nil {
		t.Error("accepted world-writable tmux binary")
	}
	// 6. Non-executable file
	noExecDir := filepath.Join(t.TempDir(), "noexec-bin")
	_ = os.MkdirAll(noExecDir, 0o755)
	noExec := filepath.Join(noExecDir, "tmux")
	_ = os.WriteFile(noExec, []byte("data"), 0o644)
	if _, err := ValidateTmuxBinary(noExec, uid); err == nil {
		t.Error("accepted non-executable tmux file")
	}
	// 7. Mismatched owner UID (if non-root)
	if uid != 0 {
		if _, err := ValidateTmuxBinary(validTmux, uid+1); err == nil {
			t.Error("accepted tmux owned by foreign uid")
		}
	}

	// CLI `serve --tmux /bin/sh` must fail with error=invalid_tmux.
	var out bytes.Buffer
	code := Main([]string{"serve", "--foreground", "--tmux", "/bin/sh"}, proto.BuildInfo{Version: "test", Release: 1}, &out, &out)
	if code == 0 || !strings.Contains(out.String(), "error=invalid_tmux") {
		t.Fatalf("serve --tmux /bin/sh: code=%d out=%q, want non-zero with error=invalid_tmux", code, out.String())
	}

	// Server with validated TmuxPath persists tmux_path into meta.json.
	sockTmp, err := os.MkdirTemp("/tmp", "cd-tm-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockTmp) })
	layout, err := ResolveLayout(PathOptions{
		Home:      t.TempDir(),
		MachineID: "abcdef012345",
		TmpBase:   sockTmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureStateDir(); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	srv, err := NewServer(ServerConfig{
		Layout:   layout,
		Build:    proto.BuildInfo{Version: "test", Release: 1},
		Gateway:  "gw.test",
		TmuxPath: validTmux,
		OnReady:  func() { close(ready) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Serve failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not become ready")
	}

	meta, err := ReadMetaRecord(layout.MetaJSON)
	if err != nil {
		t.Fatal(err)
	}
	if meta.TmuxPath != validTmux {
		t.Fatalf("meta.json tmux_path = %q, want %q", meta.TmuxPath, validTmux)
	}
	if srv.TmuxPath() != validTmux {
		t.Fatalf("srv.TmuxPath() = %q, want %q", srv.TmuxPath(), validTmux)
	}

	cancel()
	<-done

	// Verify real system tmux (if present) passes validation.
	if sysTmux, err := exec.LookPath("tmux"); err == nil {
		if _, err := ValidateTmuxBinary(sysTmux, uid); err != nil {
			t.Errorf("ValidateTmuxBinary(%s) failed: %v", sysTmux, err)
		}
	}
}
