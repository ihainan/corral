//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// makeSafeTestRoot creates a 0755 root inside t.TempDir() and injects it as
// the test-only safe-path check root so tests never depend on permissions of
// uncontrolled host directories above t.TempDir() (e.g. /home/user/tmp 0775 or
// /tmp 1777).
func makeSafeTestRoot(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "trusted-root")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	restore := SetTrustedTmuxCheckRootForTest(dir)
	t.Cleanup(restore)
	return dir
}

// Review #1, #2, #3 (Round 3): OpenTrustedTmuxBinary / ValidateTmuxBinary
// enforce safe-path validation across EVERY ancestor directory from "/" down to
// the binary (both candidate path and symlink-resolved real path), rejecting
// group- or world-writable files/directories (0022) and unmapped overflowuid
// owners; `corrald serve --tmux` validates and records tmux_path in meta.json.
func TestValidateTmuxBinaryAndServeTmuxFlag(t *testing.T) {
	uid := uint32(os.Geteuid())
	safeRoot := makeSafeTestRoot(t)

	// Safe custom directory (0755) with executable regular file named "tmux".
	safeDir := filepath.Join(safeRoot, "custom-bin")
	if err := os.MkdirAll(safeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	validTmux := filepath.Join(safeDir, "tmux")
	if err := os.WriteFile(validTmux, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(validTmux, 0o755)

	// Valid symlink (e.g. Nix profile) pointing to validTmux under safe ancestors.
	symDir := filepath.Join(safeRoot, "nix-profile", "bin")
	if err := os.MkdirAll(symDir, 0o755); err != nil {
		t.Fatal(err)
	}
	validSym := filepath.Join(symDir, "tmux")
	if err := os.Symlink(validTmux, validSym); err != nil {
		t.Fatal(err)
	}

	f, got, err := OpenTrustedTmuxBinary(validTmux, uid)
	if err != nil || got != validTmux || f == nil {
		t.Fatalf("OpenTrustedTmuxBinary(valid) = (%v, %q, %v), want (non-nil, %q, nil)", f, got, err, validTmux)
	}
	_ = f.Close()

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
	evilSym := filepath.Join(safeRoot, "evil-sym", "tmux")
	_ = os.MkdirAll(filepath.Dir(evilSym), 0o755)
	_ = os.Symlink("/bin/sh", evilSym)
	if _, err := ValidateTmuxBinary(evilSym, uid); err == nil {
		t.Error("accepted symlink named tmux pointing to /bin/sh")
	}
	// 4. World-writable parent directory
	wwDir := filepath.Join(safeRoot, "ww-bin")
	_ = os.MkdirAll(wwDir, 0o755)
	_ = os.Chmod(wwDir, 0o777)
	wwTmux := filepath.Join(wwDir, "tmux")
	_ = os.WriteFile(wwTmux, []byte("#!/bin/sh\n"), 0o755)
	_ = os.Chmod(wwTmux, 0o755)
	if _, err := ValidateTmuxBinary(wwTmux, uid); err == nil {
		t.Error("accepted tmux inside world-writable directory")
	}
	// 5. Group-writable parent directory (Round 3 #1)
	gwDir := filepath.Join(safeRoot, "gw-bin")
	_ = os.MkdirAll(gwDir, 0o755)
	_ = os.Chmod(gwDir, 0o775)
	gwDirTmux := filepath.Join(gwDir, "tmux")
	_ = os.WriteFile(gwDirTmux, []byte("#!/bin/sh\n"), 0o755)
	_ = os.Chmod(gwDirTmux, 0o755)
	if _, err := ValidateTmuxBinary(gwDirTmux, uid); err == nil {
		t.Error("accepted tmux inside group-writable (0775) directory")
	}
	// 6. Group-writable binary (Round 3 #1)
	gwBinDir := filepath.Join(safeRoot, "gw-file-bin")
	_ = os.MkdirAll(gwBinDir, 0o755)
	gwBin := filepath.Join(gwBinDir, "tmux")
	_ = os.WriteFile(gwBin, []byte("#!/bin/sh\n"), 0o755)
	_ = os.Chmod(gwBin, 0o775)
	if _, err := ValidateTmuxBinary(gwBin, uid); err == nil {
		t.Error("accepted group-writable (0775) tmux binary")
	}
	// 7. World-writable binary
	wwBinDir := filepath.Join(safeRoot, "ww-file-bin")
	_ = os.MkdirAll(wwBinDir, 0o755)
	wwBin := filepath.Join(wwBinDir, "tmux")
	_ = os.WriteFile(wwBin, []byte("#!/bin/sh\n"), 0o755)
	_ = os.Chmod(wwBin, 0o777)
	if _, err := ValidateTmuxBinary(wwBin, uid); err == nil {
		t.Error("accepted world-writable tmux binary")
	}
	// 8. Writable intermediate ancestor directory 2+ levels above (Round 3 #2)
	badAncestor := filepath.Join(safeRoot, "bad-ancestor")
	deepBinDir := filepath.Join(badAncestor, "mid", "bin")
	_ = os.MkdirAll(deepBinDir, 0o755)
	_ = os.Chmod(badAncestor, 0o775)
	deepTmux := filepath.Join(deepBinDir, "tmux")
	_ = os.WriteFile(deepTmux, []byte("#!/bin/sh\n"), 0o755)
	_ = os.Chmod(deepTmux, 0o755)
	if _, err := ValidateTmuxBinary(deepTmux, uid); err == nil {
		t.Error("accepted tmux under group-writable intermediate ancestor directory")
	}
	// 9. Safe symlink whose resolved target has a writable intermediate ancestor (Round 3 #2)
	safeLinkToBadTarget := filepath.Join(safeRoot, "link-to-bad", "tmux")
	_ = os.MkdirAll(filepath.Dir(safeLinkToBadTarget), 0o755)
	_ = os.Symlink(deepTmux, safeLinkToBadTarget)
	if _, err := ValidateTmuxBinary(safeLinkToBadTarget, uid); err == nil {
		t.Error("accepted symlink pointing to target with writable intermediate ancestor")
	}
	// 10. Non-executable file
	noExecDir := filepath.Join(safeRoot, "noexec-bin")
	_ = os.MkdirAll(noExecDir, 0o755)
	noExec := filepath.Join(noExecDir, "tmux")
	_ = os.WriteFile(noExec, []byte("data"), 0o644)
	if _, err := ValidateTmuxBinary(noExec, uid); err == nil {
		t.Error("accepted non-executable tmux file")
	}
	// 11. Mismatched owner UID (if non-root)
	if uid != 0 {
		if _, err := ValidateTmuxBinary(validTmux, uid+1); err == nil {
			t.Error("accepted tmux owned by foreign uid")
		}
	}

	// C-9: CLI `serve --tmux /bin/sh` must NOT fail with error=invalid_tmux;
	// it logs a warning and continues as if --tmux was omitted (so when not
	// installed it fails with not_installed rather than invalid_tmux).
	var out bytes.Buffer
	t.Setenv("HOME", t.TempDir())
	code := Main([]string{"serve", "--foreground", "--tmux", "/bin/sh"}, proto.BuildInfo{Version: "test", Release: 1}, &out, &out)
	if strings.Contains(out.String(), "error=invalid_tmux") {
		t.Fatalf("serve --tmux /bin/sh should ignore invalid tmux instead of returning invalid_tmux: code=%d out=%q", code, out.String())
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

	// Round 3 #3: overflowuid / unmapped owner in a user namespace must be
	// rejected (never trusted as root).
	if sysTmux, err := exec.LookPath("tmux"); err == nil {
		if info, statErr := os.Stat(sysTmux); statErr == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && st.Uid != uid {
				if _, valErr := ValidateTmuxBinary(sysTmux, uid); valErr == nil {
					t.Errorf("ValidateTmuxBinary(%s) unexpectedly accepted unmapped/foreign owner uid %d", sysTmux, st.Uid)
				}
			}
		}
	}
}

// TestOpenTrustedTmuxBinaryRejectsGroupWritableAncestor0775 explicitly verifies
// that a 0775 (group-writable) parent or higher ancestor directory — such as a
// umask-002 /home/<user>/tmp directory — is strictly rejected, and accepted
// once tightened to 0755.
func TestOpenTrustedTmuxBinaryRejectsGroupWritableAncestor0775(t *testing.T) {
	uid := uint32(os.Geteuid())
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)

	// Build root/tmp0775/work/bin/tmux where tmp0775 has mode 0775 (umask 002).
	gwAncestor := filepath.Join(root, "tmp0775")
	binDir := filepath.Join(gwAncestor, "work", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxPath := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(tmuxPath, 0o755)
	_ = os.Chmod(binDir, 0o755)
	_ = os.Chmod(filepath.Join(gwAncestor, "work"), 0o755)
	_ = os.Chmod(gwAncestor, 0o775)

	// 1. Walking from root rejects the 0775 ancestor directory.
	restore := SetTrustedTmuxCheckRootForTest(root)
	defer restore()

	f, _, err := OpenTrustedTmuxBinary(tmuxPath, uid)
	if err == nil {
		_ = f.Close()
		t.Fatalf("OpenTrustedTmuxBinary unexpectedly accepted binary under 0775 ancestor %s", gwAncestor)
	}
	if !strings.Contains(err.Error(), "writable by group or others") || !strings.Contains(err.Error(), "0775") {
		t.Fatalf("unexpected error for 0775 ancestor: %v", err)
	}

	// 2. Starting root itself being 0775 is also rejected.
	restoreGW := SetTrustedTmuxCheckRootForTest(gwAncestor)
	f2, _, err2 := OpenTrustedTmuxBinary(tmuxPath, uid)
	restoreGW()
	if err2 == nil {
		_ = f2.Close()
		t.Fatalf("OpenTrustedTmuxBinary unexpectedly accepted 0775 check root %s", gwAncestor)
	}

	// 3. With production default check root ("/"), a 0775 ancestor is also rejected.
	restoreProd := SetTrustedTmuxCheckRootForTest("/")
	f3, _, err3 := OpenTrustedTmuxBinary(tmuxPath, uid)
	restoreProd()
	if err3 == nil {
		_ = f3.Close()
		t.Fatalf("OpenTrustedTmuxBinary from / unexpectedly accepted binary under 0775 ancestor %s", gwAncestor)
	}

	// 4. Tightening the ancestor from 0775 -> 0755 makes the binary trusted.
	if err := os.Chmod(gwAncestor, 0o755); err != nil {
		t.Fatal(err)
	}
	f4, resolved, err4 := OpenTrustedTmuxBinary(tmuxPath, uid)
	if err4 != nil || f4 == nil || resolved != tmuxPath {
		t.Fatalf("OpenTrustedTmuxBinary after chmod 0755 = (%v, %q, %v), want (non-nil, %q, nil)", f4, resolved, err4, tmuxPath)
	}
	_ = f4.Close()
}

// Round 4 #1: openat(O_DIRECTORY|O_NOFOLLOW) on Linux returns ENOTDIR (not
// ELOOP) for a directory symlink such as ~/.nix-profile. Verify that:
//   - a trusted intermediate directory symlink (including chained links) is accepted,
//   - a directory symlink whose resolved target chain has a 0775 ancestor is rejected,
//   - a regular file used as an intermediate path component is rejected.
func TestOpenTrustedTmuxBinaryDirectorySymlink(t *testing.T) {
	uid := uint32(os.Geteuid())
	safeRoot := makeSafeTestRoot(t)

	// 1. Trusted Nix-style profile directory symlink chain:
	//    safeRoot/home/.nix-profile -> ../profiles/profile-1-link -> ../nix/store/tmux-3.5a
	storeBinDir := filepath.Join(safeRoot, "nix", "store", "tmux-3.5a", "bin")
	profileDir := filepath.Join(safeRoot, "profiles")
	homeDir := filepath.Join(safeRoot, "home")
	for _, d := range []string{storeBinDir, profileDir, homeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(d, 0o755)
	}
	realTmux := filepath.Join(storeBinDir, "tmux")
	if err := os.WriteFile(realTmux, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(realTmux, 0o755)

	profileLink := filepath.Join(profileDir, "profile-1-link")
	if err := os.Symlink(filepath.Join(safeRoot, "nix", "store", "tmux-3.5a"), profileLink); err != nil {
		t.Fatal(err)
	}
	nixProfile := filepath.Join(homeDir, ".nix-profile")
	if err := os.Symlink(filepath.Join("..", "profiles", "profile-1-link"), nixProfile); err != nil {
		t.Fatal(err)
	}

	viaDirSymlink := filepath.Join(nixProfile, "bin", "tmux")
	f, resolved, err := OpenTrustedTmuxBinary(viaDirSymlink, uid)
	if err != nil || f == nil || resolved != realTmux {
		t.Fatalf("OpenTrustedTmuxBinary(%s) = (%v, %q, %v), want (non-nil, %q, nil)", viaDirSymlink, f, resolved, err, realTmux)
	}
	_ = f.Close()

	// 2. Trusted directory symlink pointing into an untrusted (0775) target chain -> rejected.
	badStore := filepath.Join(safeRoot, "bad-store")
	badTargetBin := filepath.Join(badStore, "tmux-3.5a", "bin")
	if err := os.MkdirAll(badTargetBin, 0o755); err != nil {
		t.Fatal(err)
	}
	badTmux := filepath.Join(badTargetBin, "tmux")
	if err := os.WriteFile(badTmux, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(badTmux, 0o755)
	_ = os.Chmod(badStore, 0o775)

	badProfile := filepath.Join(homeDir, ".bad-nix-profile")
	if err := os.Symlink(filepath.Join(badStore, "tmux-3.5a"), badProfile); err != nil {
		t.Fatal(err)
	}
	viaBadDirSymlink := filepath.Join(badProfile, "bin", "tmux")
	if fBad, _, err := OpenTrustedTmuxBinary(viaBadDirSymlink, uid); err == nil {
		_ = fBad.Close()
		t.Fatalf("OpenTrustedTmuxBinary(%s) unexpectedly accepted directory symlink into 0775 target chain", viaBadDirSymlink)
	}

	// 3. Regular file used as an intermediate directory component -> rejected.
	notADir := filepath.Join(homeDir, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fFile, _, err := OpenTrustedTmuxBinary(filepath.Join(notADir, "tmux"), uid); err == nil {
		_ = fFile.Close()
		t.Fatal("OpenTrustedTmuxBinary accepted path with regular file as parent component")
	}
}
