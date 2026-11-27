//go:build linux

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

// Main executes a corrald CLI subcommand and returns the process exit code.
func Main(args []string, build proto.BuildInfo, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if build.Version == "" {
		build.Version = "dev"
	}
	if len(args) == 0 {
		writeErr(stdout, "usage", "subcommand required: version|manifest|selftest|install|serve|status|stop|uninstall")
		return 2
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "version":
		return cmdVersion(build, stdout)
	case "manifest":
		return cmdManifest(stdout)
	case "selftest":
		return cmdSelftest(stdout)
	case "install":
		return cmdInstall(rest, build, stdout)
	case "serve":
		return cmdServe(rest, build, stdout)
	case "status":
		return cmdStatus(stdout)
	case "stop":
		return cmdStop(stdout)
	case "uninstall":
		return cmdUninstall(rest, stdout)
	default:
		writeErr(stdout, "unknown_command", fmt.Sprintf("unknown subcommand %q", sub))
		return 2
	}
}

func resolveDefaultLayout() (*Layout, error) {
	return ResolveLayout(defaultPathOptions())
}

func writeErr(w io.Writer, code, detail string) {
	fmt.Fprintf(w, "error=%s\n", code)
	if detail != "" {
		detail = strings.ReplaceAll(detail, "\n", " ")
		fmt.Fprintf(w, "detail=%s\n", detail)
	}
}

func writeLockErr(w io.Writer, err error) int {
	if errors.Is(err, ErrLocked) {
		writeErr(w, proto.ErrBusy, err.Error())
		return 1
	}
	writeErr(w, "lock_failed", err.Error())
	return 1
}

func cmdVersion(build proto.BuildInfo, stdout io.Writer) int {
	fmt.Fprintf(stdout, "version=%s\n", build.Version)
	fmt.Fprintf(stdout, "release=%d\n", build.Release)
	fmt.Fprintf(stdout, "commit=%s\n", build.Commit)
	fmt.Fprintf(stdout, "protocol=%d\n", proto.Protocol)
	fmt.Fprintf(stdout, "arch=%s\n", runtime.GOARCH)
	fmt.Fprintf(stdout, "result=ok\n")
	return 0
}

func cmdManifest(stdout io.Writer) int {
	b, err := proto.MarshalManifest(proto.DefaultManifest())
	if err != nil {
		writeErr(stdout, "manifest_failed", err.Error())
		return 1
	}
	_, _ = stdout.Write(append(b, '\n'))
	return 0
}

func cmdSelftest(stdout io.Writer) int {
	if runtime.GOOS != "linux" {
		writeErr(stdout, "unsupported_os", runtime.GOOS)
		return 1
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		writeErr(stdout, "unsupported_arch", runtime.GOARCH)
		return 1
	}

	layout, err := resolveDefaultLayout()
	if err != nil {
		writeErr(stdout, "layout_failed", err.Error())
		return 1
	}
	if err := layout.EnsureStateDir(); err != nil {
		writeErr(stdout, "state_dir_failed", err.Error())
		return 1
	}

	probeLockPath := filepath.Join(layout.StateDir, fmt.Sprintf(".selftest-%d.lock", os.Getpid()))
	lock, err := AcquireLock(probeLockPath)
	if err != nil {
		writeErr(stdout, "flock_failed", err.Error())
		return 1
	}
	_ = os.Remove(probeLockPath)
	_ = lock.Release()

	if err := layout.EnsureSocketDir(); err != nil {
		writeErr(stdout, "socket_dir_failed", err.Error())
		return 1
	}
	probeSock := filepath.Join(layout.SocketDir, fmt.Sprintf(".selftest-%d.sock", os.Getpid()))
	_ = os.Remove(probeSock)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: probeSock, Net: "unix"})
	if err != nil {
		writeErr(stdout, "socket_bind_failed", err.Error())
		return 1
	}
	_ = ln.Close()
	_ = os.Remove(probeSock)

	fmt.Fprintf(stdout, "result=ok\n")
	fmt.Fprintf(stdout, "mid=%s\n", layout.MachineID)
	fmt.Fprintf(stdout, "mid_source=%s\n", layout.MachineIDSource)
	fmt.Fprintf(stdout, "state_dir=%s\n", layout.StateDir)
	fmt.Fprintf(stdout, "socket=%s\n", layout.SocketPath)
	return 0
}

func cmdInstall(args []string, build proto.BuildInfo, stdout io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stageDir := fs.String("stage", "", "staging directory (<state_dir>/stage-<op>) containing corrald binary")
	opID := fs.String("op", "", "operation identifier")
	gateway := fs.String("gateway", "", "gateway identifier")
	by := fs.String("by", "", "enabling user")
	upgrade := fs.Bool("upgrade", false, "stage new version without switching symlink")
	if err := fs.Parse(args); err != nil {
		writeErr(stdout, "invalid_args", err.Error())
		return 2
	}
	if *stageDir == "" || *opID == "" || *gateway == "" || *by == "" {
		writeErr(stdout, "invalid_args", "required flags: --stage, --op, --gateway, --by")
		return 2
	}

	layout, err := resolveDefaultLayout()
	if err != nil {
		writeErr(stdout, "layout_failed", err.Error())
		return 1
	}

	expectedStage, err := layout.StageDir(*opID)
	if err != nil {
		writeErr(stdout, "invalid_op", err.Error())
		return 2
	}
	absStage, err := filepath.Abs(*stageDir)
	if err != nil || filepath.Clean(absStage) != filepath.Clean(expectedStage) {
		writeErr(stdout, "invalid_stage", fmt.Sprintf("--stage must be %s", expectedStage))
		return 2
	}

	if err := layout.EnsureLibDir(); err != nil {
		writeErr(stdout, "lib_dir_failed", err.Error())
		return 1
	}
	if err := layout.EnsureStateDir(); err != nil {
		writeErr(stdout, "state_dir_failed", err.Error())
		return 1
	}

	lockTimeout := defaultLifecycleLockTimeout()
	sharedLock, err := AcquireLockWait(layout.SharedLockPath, lockTimeout)
	if err != nil {
		return writeLockErr(stdout, err)
	}
	defer sharedLock.Release()

	opLock, err := AcquireLockWait(layout.OpLockPath, lockTimeout)
	if err != nil {
		return writeLockErr(stdout, err)
	}
	defer opLock.Release()

	if layout.TombstoneExists() {
		writeErr(stdout, proto.ErrRemoving, "uninstall tombstone present")
		return 1
	}

	if err := EnsureSecureDir(expectedStage, layout.UID); err != nil {
		writeErr(stdout, "invalid_stage", err.Error())
		return 1
	}
	srcPath := filepath.Join(expectedStage, "corrald")
	srcInfo, err := os.Lstat(srcPath)
	if err != nil || srcInfo.Mode()&os.ModeSymlink != 0 || !srcInfo.Mode().IsRegular() {
		writeErr(stdout, "stage_missing", fmt.Sprintf("staged regular binary not found at %s", srcPath))
		return 1
	}
	if st, ok := srcInfo.Sys().(*syscall.Stat_t); !ok || st.Uid != layout.UID {
		writeErr(stdout, "invalid_stage", "staged binary is not owned by current user")
		return 1
	}

	verDir, err := layout.VersionDir(build.Version, build.Release)
	if err != nil {
		writeErr(stdout, "invalid_version", err.Error())
		return 1
	}
	if err := os.Mkdir(verDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		writeErr(stdout, "mkdir_failed", err.Error())
		return 1
	}
	if err := EnsureSecureDir(verDir, layout.UID); err != nil {
		writeErr(stdout, "ver_dir_invalid", err.Error())
		return 1
	}
	dstBin := filepath.Join(verDir, "corrald")
	if err := copyBinaryAtomic(srcPath, dstBin); err != nil {
		writeErr(stdout, "copy_failed", err.Error())
		return 1
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)

	var installRec *InstallRecord
	if *upgrade {
		installRec, _ = ReadInstallRecord(layout.InstallJSON)
		if installRec == nil {
			installRec = &InstallRecord{
				Version:         build.Version,
				Release:         build.Release,
				Commit:          build.Commit,
				Gateway:         *gateway,
				EnabledBy:       *by,
				EnabledAt:       nowStr,
				MachineID:       layout.MachineID,
				MachineIDSource: layout.MachineIDSource,
			}
		}
		installRec.Files = append(installRec.Files, verDir, dstBin, layout.CurrentSymlink, layout.ReadmePath, layout.StateDir, layout.InstallJSON)
		if err := WriteInstallRecord(layout.InstallJSON, installRec); err != nil {
			writeErr(stdout, "install_json_failed", err.Error())
			return 1
		}
	} else {
		relTarget, err := filepath.Rel(layout.LibDir, dstBin)
		if err != nil {
			writeErr(stdout, "symlink_failed", err.Error())
			return 1
		}
		if err := AtomicSymlink(relTarget, layout.CurrentSymlink); err != nil {
			writeErr(stdout, "symlink_failed", err.Error())
			return 1
		}
		files := []string{
			verDir,
			dstBin,
			layout.CurrentSymlink,
			layout.ReadmePath,
			layout.StateDir,
			layout.InstallJSON,
			layout.LockPath,
			layout.OpLockPath,
			layout.MetaJSON,
			layout.ClientsJSON,
			layout.LogPath,
			layout.OpsDir,
			layout.SocketPath,
		}
		installRec = &InstallRecord{
			Version:         build.Version,
			Release:         build.Release,
			Commit:          build.Commit,
			Gateway:         *gateway,
			EnabledBy:       *by,
			EnabledAt:       nowStr,
			MachineID:       layout.MachineID,
			MachineIDSource: layout.MachineIDSource,
			Files:           files,
		}
		if err := WriteInstallRecord(layout.InstallJSON, installRec); err != nil {
			writeErr(stdout, "install_json_failed", err.Error())
			return 1
		}
	}

	readmeText := proto.RenderReadme(proto.ReadmeData{
		Version:    installRec.Version,
		Release:    installRec.Release,
		Gateway:    installRec.Gateway,
		EnabledBy:  installRec.EnabledBy,
		EnabledAt:  installRec.EnabledAt,
		MachineID:  layout.MachineID,
		LibDir:     layout.LibDir,
		StateDir:   layout.StateDir,
		SocketPath: layout.SocketPath,
		Files:      installRec.Files,
	})
	if err := WriteFileAtomic(layout.ReadmePath, []byte(readmeText), 0o644); err != nil {
		writeErr(stdout, "readme_failed", err.Error())
		return 1
	}

	opPath, err := layout.OpPath(*opID)
	if err != nil {
		writeErr(stdout, "invalid_op", err.Error())
		return 1
	}
	kind := "install"
	if *upgrade {
		kind = "upgrade"
	}
	if err := WriteOpRecord(opPath, &OpRecordFile{
		ID:      *opID,
		Kind:    kind,
		Version: build.Version,
		Release: build.Release,
		Gateway: *gateway,
		By:      *by,
		At:      nowStr,
		Result:  "ok",
	}); err != nil {
		writeErr(stdout, "op_record_failed", err.Error())
		return 1
	}

	if err := os.RemoveAll(expectedStage); err != nil {
		writeErr(stdout, "stage_cleanup_failed", err.Error())
		return 1
	}

	fmt.Fprintf(stdout, "result=ok\n")
	fmt.Fprintf(stdout, "version=%s\n", build.Version)
	fmt.Fprintf(stdout, "release=%d\n", build.Release)
	fmt.Fprintf(stdout, "binary=%s\n", dstBin)
	fmt.Fprintf(stdout, "symlink=%s\n", layout.CurrentSymlink)
	fmt.Fprintf(stdout, "state_dir=%s\n", layout.StateDir)
	return 0
}

func copyBinaryAtomic(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if info, err := in.Stat(); err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("source binary %s is not a regular file", src)
	}

	dir := filepath.Dir(dst)
	if dinfo, err := os.Lstat(dir); err != nil {
		return err
	} else if dinfo.Mode()&os.ModeSymlink != 0 || !dinfo.IsDir() {
		return fmt.Errorf("destination dir %s is not a real directory", dir)
	}
	if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to overwrite symlink %s", dst)
	}

	tmp := filepath.Join(dir, fmt.Sprintf(".corrald.tmp.%d.%s", os.Getpid(), RandomHex(4)))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o700)
	if err != nil {
		return err
	}
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(tmp)
	}
	if _, err := io.Copy(out, in); err != nil {
		cleanup()
		return err
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := out.Chmod(0o700); err != nil {
		cleanup()
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// checkServeAllowed verifies Rule 4's startup preconditions under OpLock:
// no removing tombstone AND install.json must exist.
func checkServeAllowed(layout *Layout) (code string, detail string, ok bool) {
	if layout.TombstoneExists() {
		return proto.ErrRemoving, "uninstall tombstone present", false
	}
	if _, err := os.Stat(layout.InstallJSON); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "not_installed", "install.json not found", false
		}
		return "install_json_failed", err.Error(), false
	}
	return "", "", true
}

func cmdServe(args []string, build proto.BuildInfo, stdout io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gateway := fs.String("gateway", "", "gateway address shown in process list")
	foreground := fs.Bool("foreground", false, "run in foreground without detaching")
	handoff := fs.Bool("handoff", false, "internal: adopt inherited fds during hot update")
	listenFD := fs.Int("listen-fd", -1, "internal: inherited Unix listener fd")
	lockFD := fs.Int("lock-fd", -1, "internal: inherited flock fd")
	stateFD := fs.Int("state-fd", -1, "internal: inherited handoff state pipe fd")
	readyFD := fs.Int("ready-fd", -1, "internal: readiness notification pipe fd")
	binFD := fs.Int("bin-fd", -1, "internal: inherited validated binary fd to close")
	binaryPath := fs.String("binary-path", "", "internal: validated binary path for handoff cutover")
	if err := fs.Parse(args); err != nil {
		writeErr(stdout, "invalid_args", err.Error())
		return 2
	}

	if *binFD >= 0 {
		_ = syscall.Close(*binFD)
	}

	layout, err := resolveDefaultLayout()
	if err != nil {
		writeErr(stdout, "layout_failed", err.Error())
		return 1
	}

	if layout.TombstoneExists() {
		writeErr(stdout, proto.ErrRemoving, "uninstall tombstone present")
		return 1
	}

	if !*handoff {
		if _, err := os.Lstat(layout.StateDir); errors.Is(err, os.ErrNotExist) {
			writeErr(stdout, "not_installed", "install.json not found")
			return 1
		}
	}

	if err := layout.EnsureStateDir(); err != nil {
		writeErr(stdout, "state_dir_failed", err.Error())
		return 1
	}

	if !*foreground {
		return serveDetached(layout, *gateway, stdout)
	}

	return serveForeground(layout, build, *gateway, *handoff, *listenFD, *lockFD, *stateFD, *readyFD, *binaryPath, stdout)
}

func serveDetached(layout *Layout, gateway string, stdout io.Writer) int {
	opLock, err := AcquireLockWait(layout.OpLockPath, defaultLifecycleLockTimeout())
	if err != nil {
		return writeLockErr(stdout, err)
	}
	if code, detail, ok := checkServeAllowed(layout); !ok {
		_ = opLock.Release()
		writeErr(stdout, code, detail)
		return 1
	}
	probeLock, err := AcquireLock(layout.LockPath)
	if errors.Is(err, ErrLocked) {
		_ = opLock.Release()
		pid, sock := waitForRunningMeta(layout, 2*time.Second)
		fmt.Fprintf(stdout, "result=running\n")
		fmt.Fprintf(stdout, "pid=%d\n", pid)
		fmt.Fprintf(stdout, "socket=%s\n", sock)
		return 0
	}
	if err != nil {
		_ = opLock.Release()
		writeErr(stdout, "lock_failed", err.Error())
		return 1
	}
	_ = probeLock.Release()
	_ = opLock.Release()

	exe, err := os.Executable()
	if err != nil {
		writeErr(stdout, "exec_path_failed", err.Error())
		return 1
	}

	readyR, readyW, err := os.Pipe()
	if err != nil {
		writeErr(stdout, "pipe_failed", err.Error())
		return 1
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		writeErr(stdout, "devnull_failed", err.Error())
		return 1
	}
	logFile, err := OpenLogFileNoFollow(layout.LogPath)
	if err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		_ = devNull.Close()
		writeErr(stdout, "log_open_failed", err.Error())
		return 1
	}

	argv := []string{exe, "serve"}
	if gateway != "" {
		argv = append(argv, "--gateway", gateway)
	}
	argv = append(argv, "--foreground", "--ready-fd=3")

	proc, err := os.StartProcess(exe, argv, &os.ProcAttr{
		Files: []*os.File{devNull, logFile, logFile, readyW},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	_ = devNull.Close()
	_ = logFile.Close()
	_ = readyW.Close()
	if err != nil {
		_ = readyR.Close()
		writeErr(stdout, "spawn_failed", err.Error())
		return 1
	}

	type readyMsg struct {
		line string
		err  error
	}
	ch := make(chan readyMsg, 1)
	go func() {
		sc := bufio.NewScanner(readyR)
		if sc.Scan() {
			ch <- readyMsg{line: sc.Text()}
			return
		}
		if err := sc.Err(); err != nil {
			ch <- readyMsg{err: err}
			return
		}
		ch <- readyMsg{err: io.EOF}
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case msg := <-ch:
		_ = readyR.Close()
		if msg.err != nil {
			writeErr(stdout, "start_failed", fmt.Sprintf("daemon exited before ready: %v", msg.err))
			return 1
		}
		fields := strings.Fields(msg.line)
		if len(fields) >= 3 && (fields[0] == "READY" || fields[0] == "RUNNING") {
			res := "ok"
			if fields[0] == "RUNNING" {
				res = "running"
			}
			fmt.Fprintf(stdout, "result=%s\n", res)
			fmt.Fprintf(stdout, "pid=%s\n", fields[1])
			fmt.Fprintf(stdout, "socket=%s\n", fields[2])
			return 0
		}
		if len(fields) >= 2 && fields[0] == "ERROR" {
			detail := ""
			if len(fields) > 2 {
				detail = strings.Join(fields[2:], " ")
			}
			writeErr(stdout, fields[1], detail)
			return 1
		}
		writeErr(stdout, "start_failed", fmt.Sprintf("unexpected child response: %q", msg.line))
		return 1
	case <-timer.C:
		_ = readyR.Close()
		_ = proc.Kill()
		_, _ = proc.Wait()
		writeErr(stdout, "start_timeout", "daemon did not become ready within 5s")
		return 1
	}
}

func serveForeground(
	layout *Layout,
	build proto.BuildInfo,
	gateway string,
	handoff bool,
	listenFD, lockFD, stateFD, readyFD int,
	binaryPath string,
	stdout io.Writer,
) int {
	var readyFile *os.File
	if readyFD >= 0 {
		readyFile = os.NewFile(uintptr(readyFD), "ready-pipe")
	}
	reportReadyErr := func(code, detail string) int {
		if readyFile != nil {
			_, _ = fmt.Fprintf(readyFile, "ERROR %s %s\n", code, strings.ReplaceAll(detail, "\n", " "))
			_ = readyFile.Close()
			readyFile = nil
		}
		writeErr(stdout, code, detail)
		return 1
	}

	if shouldFailBeforeReady(handoff) {
		if readyFile != nil {
			_ = readyFile.Close()
		}
		return 1
	}

	rotLog, err := OpenRotatingWriter(layout.LogPath, DefaultMaxLogBytes)
	if err != nil {
		return reportReadyErr("log_open_failed", err.Error())
	}
	defer rotLog.Close()
	logger := NewLogger(rotLog)

	var (
		lock   *FileLock
		opLock *FileLock
	)
	releaseOpLock := func() {
		if opLock != nil {
			_ = opLock.Release()
			opLock = nil
		}
	}
	defer releaseOpLock()

	if handoff {
		// Parent v1 already holds SharedLock -> OpLock across the entire
		// handoff transaction; child never re-acquires OpLock and adopts the
		// inherited corrald.lock FD when present.
		if layout.TombstoneExists() {
			return reportReadyErr(proto.ErrRemoving, "uninstall tombstone present")
		}
		if lockFD >= 0 {
			lock, err = AdoptLock(uintptr(lockFD))
			if err != nil {
				return reportReadyErr("adopt_lock_failed", err.Error())
			}
		}
	} else {
		opLock, err = AcquireLockWait(layout.OpLockPath, defaultLifecycleLockTimeout())
		if err != nil {
			if errors.Is(err, ErrLocked) {
				return reportReadyErr(proto.ErrBusy, err.Error())
			}
			return reportReadyErr("lock_failed", err.Error())
		}
		if code, detail, ok := checkServeAllowed(layout); !ok {
			return reportReadyErr(code, detail)
		}
		lock, err = AcquireLock(layout.LockPath)
		if errors.Is(err, ErrLocked) {
			releaseOpLock()
			pid, sock := waitForRunningMeta(layout, 2*time.Second)
			if readyFile != nil {
				_, _ = fmt.Fprintf(readyFile, "RUNNING %d %s\n", pid, sock)
				_ = readyFile.Close()
				return 0
			}
			fmt.Fprintf(stdout, "result=running\n")
			fmt.Fprintf(stdout, "pid=%d\n", pid)
			fmt.Fprintf(stdout, "socket=%s\n", sock)
			return 0
		}
		if err != nil {
			return reportReadyErr("lock_failed", err.Error())
		}
		if code, detail, ok := checkServeAllowed(layout); !ok {
			_ = lock.Release()
			return reportReadyErr(code, detail)
		}
	}
	if lock != nil {
		defer lock.Release()
	}

	var (
		ln   *net.UnixListener
		snap *HandoffSnapshot
	)
	if handoff {
		if stateFD < 0 {
			return reportReadyErr("snapshot_invalid", "missing state pipe fd")
		}
		sf := os.NewFile(uintptr(stateFD), "state-pipe")
		rawSnap, rerr := io.ReadAll(io.LimitReader(sf, MaxHandoffReadBytes+1))
		_ = sf.Close()
		if rerr != nil {
			return reportReadyErr("snapshot_invalid", fmt.Sprintf("read state pipe: %v", rerr))
		}
		if len(rawSnap) == 0 {
			return reportReadyErr("snapshot_invalid", "empty handoff snapshot")
		}
		if len(rawSnap) > MaxHandoffReadBytes {
			return reportReadyErr("snapshot_invalid", fmt.Sprintf("handoff snapshot exceeds %d bytes", MaxHandoffReadBytes))
		}
		var parsed HandoffSnapshot
		if jerr := json.Unmarshal(rawSnap, &parsed); jerr != nil {
			return reportReadyErr("snapshot_invalid", fmt.Sprintf("parse handoff snapshot: %v", jerr))
		}
		if parsed.Schema != HandoffSnapshotSchema {
			return reportReadyErr("snapshot_invalid", fmt.Sprintf("unsupported snapshot schema %d (want %d)", parsed.Schema, HandoffSnapshotSchema))
		}
		snap = &parsed
		if snap.SocketPath != "" {
			layout.SocketPath = snap.SocketPath
			layout.SocketDir = filepath.Dir(snap.SocketPath)
		}
		if gateway == "" && snap.Gateway != "" {
			gateway = snap.Gateway
		}

		if listenFD >= 0 {
			lf := os.NewFile(uintptr(listenFD), "listen-sock")
			fl, ferr := net.FileListener(lf)
			_ = lf.Close()
			if ferr != nil {
				return reportReadyErr("adopt_listen_failed", ferr.Error())
			}
			ul, ok := fl.(*net.UnixListener)
			if !ok {
				_ = fl.Close()
				return reportReadyErr("adopt_listen_failed", "inherited listener is not unix socket")
			}
			ln = ul
		}
	}

	cfg := ServerConfig{
		Layout:          layout,
		Build:           build,
		Gateway:         gateway,
		Logger:          logger,
		Lock:            lock,
		Listener:        ln,
		InitialSnapshot: snap,
		OnHardExit: func() {
			os.Exit(0)
		},
	}
	applyTestServerConfig(&cfg)

	if handoff {
		cfg.PreReady = func(meta *MetaRecord) error {
			return finalizeHandoffCutover(layout, build, binaryPath, meta)
		}
	}

	cfg.OnReady = func() {
		releaseOpLock()
		if readyFile != nil {
			if handoff {
				procStart, _, _ := ProcStartTime(os.Getpid())
				_, _ = fmt.Fprintf(readyFile, "READY %d %s %d\n", os.Getpid(), procStart, build.Release)
			} else {
				_, _ = fmt.Fprintf(readyFile, "READY %d %s\n", os.Getpid(), layout.SocketPath)
			}
			_ = readyFile.Close()
			readyFile = nil
		}
	}

	srv, err := NewServer(cfg)
	if err != nil {
		if build.Release > 0 {
			_ = RecordFailedRelease(layout.FailedJSON, build.Release, err.Error())
		}
		return reportReadyErr("server_init_failed", err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := srv.Serve(ctx); err != nil {
		if build.Release > 0 {
			_ = RecordFailedRelease(layout.FailedJSON, build.Release, err.Error())
		}
		return reportReadyErr("serve_failed", err.Error())
	}
	return 0
}

// finalizeHandoffCutover executes inside the parent v1's already-held
// SharedLock -> OpLock critical section. It verifies the target binary exists
// on disk and updates symlink, install.json, README.txt, and meta.json BEFORE
// Accept starts and BEFORE READY is signaled. If any step fails, it returns an
// error without signaling READY and v1 unconditionally restores its backup.
func finalizeHandoffCutover(layout *Layout, build proto.BuildInfo, binaryPath string, meta *MetaRecord) error {
	if shouldFailCutover() {
		return errors.New("injected cutover failure")
	}

	if layout.TombstoneExists() {
		return errors.New("uninstall tombstone present")
	}

	exe := binaryPath
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable: %w", err)
		}
	}
	cleanExe := filepath.Clean(exe)
	exeInfo, err := os.Lstat(cleanExe)
	if err != nil {
		return fmt.Errorf("cutover target missing %s: %w", cleanExe, err)
	}
	if exeInfo.Mode()&os.ModeSymlink != 0 || !exeInfo.Mode().IsRegular() {
		return fmt.Errorf("cutover target %s is not a regular file", cleanExe)
	}
	if st, ok := exeInfo.Sys().(*syscall.Stat_t); !ok || st.Uid != layout.UID {
		return fmt.Errorf("cutover target %s ownership invalid", cleanExe)
	}
	if exeInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("cutover target %s mode is %04o, want 0700", cleanExe, exeInfo.Mode().Perm())
	}

	realLib, err := filepath.EvalSymlinks(layout.LibDir)
	if err != nil {
		realLib = filepath.Clean(layout.LibDir)
	}
	realExe, err := filepath.EvalSymlinks(cleanExe)
	if err != nil {
		return fmt.Errorf("resolve target %s: %w", cleanExe, err)
	}
	rel, err := filepath.Rel(realLib, realExe)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("replacement binary %s outside lib dir %s", realExe, realLib)
	}

	if err := AtomicSymlink(rel, layout.CurrentSymlink); err != nil {
		return fmt.Errorf("switch symlink: %w", err)
	}

	if rec, err := ReadInstallRecord(layout.InstallJSON); err == nil {
		rec.Version = build.Version
		rec.Release = build.Release
		rec.Commit = build.Commit
		rec.Files = append(rec.Files, filepath.Dir(realExe), realExe)
		if err := WriteInstallRecord(layout.InstallJSON, rec); err != nil {
			return fmt.Errorf("update install.json: %w", err)
		}
		readmeText := proto.RenderReadme(proto.ReadmeData{
			Version:    rec.Version,
			Release:    rec.Release,
			Gateway:    rec.Gateway,
			EnabledBy:  rec.EnabledBy,
			EnabledAt:  rec.EnabledAt,
			MachineID:  layout.MachineID,
			LibDir:     layout.LibDir,
			StateDir:   layout.StateDir,
			SocketPath: layout.SocketPath,
			Files:      rec.Files,
		})
		if err := WriteFileAtomic(layout.ReadmePath, []byte(readmeText), 0o644); err != nil {
			return fmt.Errorf("update README.txt: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read install.json: %w", err)
	}

	if err := WriteMetaRecord(layout.MetaJSON, meta); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}
	return nil
}

func waitForRunningMeta(layout *Layout, wait time.Duration) (int, string) {
	deadline := time.Now().Add(wait)
	for {
		if meta, err := ReadMetaRecord(layout.MetaJSON); err == nil && meta.PID > 0 {
			sock := meta.SocketPath
			if sock == "" {
				sock = layout.SocketPath
			}
			return meta.PID, sock
		}
		if time.Now().After(deadline) {
			return 0, layout.SocketPath
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func cmdStatus(stdout io.Writer) int {
	layout, err := resolveDefaultLayout()
	if err != nil {
		writeErr(stdout, "layout_failed", err.Error())
		return 1
	}

	meta, metaErr := ReadMetaRecord(layout.MetaJSON)
	sockPath := layout.SocketPath
	if metaErr == nil && meta.SocketPath != "" {
		sockPath = meta.SocketPath
	}

	var health proto.HealthResult
	if err := callLocalRPC(sockPath, proto.MethodDaemonHealth, nil, &health, 2*time.Second); err == nil {
		state := health.State
		if state == "" {
			state = proto.StateRunning
		}
		fmt.Fprintf(stdout, "result=ok\n")
		fmt.Fprintf(stdout, "state=%s\n", state)
		fmt.Fprintf(stdout, "version=%s\n", health.Version)
		fmt.Fprintf(stdout, "release=%d\n", health.Release)
		fmt.Fprintf(stdout, "pid=%d\n", health.PID)
		fmt.Fprintf(stdout, "socket=%s\n", health.Socket)
		fmt.Fprintf(stdout, "gateways=%d\n", health.ConnectedGateways)
		fmt.Fprintf(stdout, "rss_kb=%d\n", health.RSSKB)
		fmt.Fprintf(stdout, "uptime_s=%d\n", health.UptimeS)
		return 0
	}

	if metaErr == nil && ProcessMatches(meta.PID, meta.StartTime) {
		fmt.Fprintf(stdout, "result=ok\n")
		fmt.Fprintf(stdout, "state=running\n")
		fmt.Fprintf(stdout, "version=%s\n", meta.Version)
		fmt.Fprintf(stdout, "release=%d\n", meta.Release)
		fmt.Fprintf(stdout, "pid=%d\n", meta.PID)
		fmt.Fprintf(stdout, "socket=%s\n", sockPath)
		fmt.Fprintf(stdout, "gateways=0\n")
		fmt.Fprintf(stdout, "rss_kb=%d\n", ProcRSSKB(meta.PID))
		return 0
	}

	installRec, instErr := ReadInstallRecord(layout.InstallJSON)
	fmt.Fprintf(stdout, "result=ok\n")
	if instErr != nil {
		fmt.Fprintf(stdout, "state=not_installed\n")
	} else {
		fmt.Fprintf(stdout, "state=stopped\n")
		fmt.Fprintf(stdout, "version=%s\n", installRec.Version)
		fmt.Fprintf(stdout, "release=%d\n", installRec.Release)
	}
	fmt.Fprintf(stdout, "pid=0\n")
	fmt.Fprintf(stdout, "socket=%s\n", sockPath)
	fmt.Fprintf(stdout, "gateways=0\n")
	fmt.Fprintf(stdout, "rss_kb=0\n")
	return 0
}

func cmdStop(stdout io.Writer) int {
	layout, err := resolveDefaultLayout()
	if err != nil {
		writeErr(stdout, "layout_failed", err.Error())
		return 1
	}
	stopped, pid, err := StopDaemon(layout)
	if err != nil {
		fmt.Fprintf(stdout, "error=stop_failed\n")
		fmt.Fprintf(stdout, "pid=%d\n", pid)
		fmt.Fprintf(stdout, "detail=%s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return 1
	}
	fmt.Fprintf(stdout, "result=ok\n")
	if stopped {
		fmt.Fprintf(stdout, "stopped=true\n")
	} else {
		fmt.Fprintf(stdout, "already_stopped=true\n")
	}
	return 0
}

// StopDaemon stops all running corrald processes for layout (including a
// post-handoff draining predecessor recorded in meta.PrevPID) and verifies
// corrald.lock is no longer held by any process before cleaning up socket/meta
// artifacts.
func StopDaemon(layout *Layout) (stopped bool, pid int, err error) {
	meta, metaErr := ReadMetaRecord(layout.MetaJSON)
	sockPath := layout.SocketPath
	if metaErr == nil && meta.SocketPath != "" {
		sockPath = meta.SocketPath
	}

	wasRunning := metaErr == nil && ProcessMatches(meta.PID, meta.StartTime)
	if metaErr == nil {
		pid = meta.PID
	}

	var res proto.ShutdownResult
	rpcOK := callLocalRPC(sockPath, proto.MethodDaemonShutdown, proto.ShutdownParams{Reason: "stop"}, &res, 2*time.Second) == nil
	if rpcOK {
		wasRunning = true
		if meta != nil {
			_ = waitSpecificProcessGone(meta.PID, meta.StartTime, 3*time.Second)
		} else {
			_ = waitLockFree(layout.LockPath, 3*time.Second)
		}
	}

	if freshMeta, err := ReadMetaRecord(layout.MetaJSON); err == nil {
		meta = freshMeta
		metaErr = nil
		pid = meta.PID
		if meta.SocketPath != "" {
			sockPath = meta.SocketPath
		}
	}

	if metaErr == nil && ProcessMatches(meta.PID, meta.StartTime) {
		wasRunning = true
		if err := terminateVerifiedProcess(meta.PID, meta.StartTime); err != nil {
			return false, meta.PID, err
		}
	}

	if metaErr == nil && meta.PrevPID > 0 && ProcessMatches(meta.PrevPID, meta.PrevStartTime) {
		wasRunning = true
		if err := terminateVerifiedProcess(meta.PrevPID, meta.PrevStartTime); err != nil {
			return false, meta.PrevPID, err
		}
	}

	if lockHeldErr := ensureLockReleased(layout); lockHeldErr != nil {
		return false, pid, lockHeldErr
	}

	cleanupStoppedArtifacts(layout, sockPath)
	return wasRunning, 0, nil
}

func terminateVerifiedProcess(pid int, startTime string) error {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if waitSpecificProcessGone(pid, startTime, 3*time.Second) {
		return nil
	}
	if !ProcessMatches(pid, startTime) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if waitSpecificProcessGone(pid, startTime, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("process %d did not exit after SIGTERM and SIGKILL", pid)
}

func ensureLockReleased(layout *Layout) error {
	if _, err := os.Lstat(layout.LockPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		lock, err := AcquireLock(layout.LockPath)
		if err == nil {
			_ = lock.Release()
			return nil
		}
		if !errors.Is(err, ErrLocked) {
			return fmt.Errorf("verify lock %s: %w", layout.LockPath, err)
		}
		if meta, merr := ReadMetaRecord(layout.MetaJSON); merr == nil {
			if ProcessMatches(meta.PID, meta.StartTime) {
				if kerr := terminateVerifiedProcess(meta.PID, meta.StartTime); kerr != nil {
					return kerr
				}
				continue
			}
			if meta.PrevPID > 0 && ProcessMatches(meta.PrevPID, meta.PrevStartTime) {
				if kerr := terminateVerifiedProcess(meta.PrevPID, meta.PrevStartTime); kerr != nil {
					return kerr
				}
				continue
			}
		}
		if time.Now().After(deadline) {
			return errors.New("corrald.lock is still held by an active process")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func waitSpecificProcessGone(pid int, startTime string, timeout time.Duration) bool {
	if pid <= 0 || startTime == "" {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		if !ProcessMatches(pid, startTime) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func waitLockFree(lockPath string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		lock, err := AcquireLock(lockPath)
		if err == nil {
			_ = lock.Release()
			return true
		}
		if !errors.Is(err, ErrLocked) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func cleanupStoppedArtifacts(layout *Layout, sockPath string) {
	if sockPath != "" {
		_ = os.Remove(sockPath)
	}
	if layout.SocketPath != "" && layout.SocketPath != sockPath {
		_ = os.Remove(layout.SocketPath)
	}
	_ = os.Remove(layout.MetaJSON)
}

func cmdUninstall(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opID := fs.String("op", "", "optional operation identifier")
	if err := fs.Parse(args); err != nil {
		writeErr(stdout, "invalid_args", err.Error())
		return 2
	}
	_ = opID

	layout, err := resolveDefaultLayout()
	if err != nil {
		writeErr(stdout, "layout_failed", err.Error())
		return 1
	}

	lockTimeout := defaultLifecycleLockTimeout()
	var (
		sharedLock *FileLock
		opLock     *FileLock
		lifeLock   *FileLock
	)

	// Rule 1: Acquire SharedLock FIRST (if LibDir exists). Never cache
	// StateDir, install.json, or other-machine checks from before SharedLock!
	libDirExists := false
	if _, err := os.Lstat(layout.LibDir); err == nil {
		libDirExists = true
		sharedLock, err = AcquireLockWait(layout.SharedLockPath, lockTimeout)
		if err != nil {
			return writeLockErr(stdout, err)
		}
		defer sharedLock.Release()
	} else if !errors.Is(err, os.ErrNotExist) {
		writeErr(stdout, "lib_dir_failed", err.Error())
		return 1
	}

	// Re-check StateDir existence AFTER acquiring SharedLock (Rule 4).
	stateDirExists := false
	if _, err := os.Lstat(layout.StateDir); err == nil {
		stateDirExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		writeErr(stdout, "state_dir_failed", err.Error())
		return 1
	}

	var (
		metaBefore *MetaRecord
		installRec *InstallRecord
		installErr error
	)
	if stateDirExists {
		opLock, err = AcquireLockWait(layout.OpLockPath, lockTimeout)
		if err != nil {
			return writeLockErr(stdout, err)
		}
		defer opLock.Release()

		metaBefore, _ = ReadMetaRecord(layout.MetaJSON)
		installRec, installErr = ReadInstallRecord(layout.InstallJSON)

		// Rule 4: Under OpLock, first remove install.json, then write the
		// removing tombstone. If either step fails, abort before stopping the
		// running daemon or touching other files.
		if err := os.Remove(layout.InstallJSON); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeErr(stdout, "remove_install_json_failed", err.Error())
			return 1
		}
		if err := WriteFileAtomic(layout.TombstonePath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			writeErr(stdout, "tombstone_failed", err.Error())
			return 1
		}
	} else {
		installErr = os.ErrNotExist
	}

	sockPath := layout.SocketPath
	if metaBefore != nil && metaBefore.SocketPath != "" {
		sockPath = metaBefore.SocketPath
	}

	// Stop running daemon (and any draining predecessor) and confirm lock is free.
	_, pid, stopErr := StopDaemon(layout)
	if stopErr != nil {
		fmt.Fprintf(stdout, "error=stop_failed\n")
		fmt.Fprintf(stdout, "pid=%d\n", pid)
		fmt.Fprintf(stdout, "detail=%s\n", strings.ReplaceAll(stopErr.Error(), "\n", " "))
		return 1
	}

	if stateDirExists {
		lifeLock, err = AcquireLock(layout.LockPath)
		if err != nil {
			fmt.Fprintf(stdout, "error=stop_failed\n")
			fmt.Fprintf(stdout, "pid=0\n")
			fmt.Fprintf(stdout, "detail=%s\n", strings.ReplaceAll(err.Error(), "\n", " "))
			return 1
		}
		defer lifeLock.Release()
	}

	type leftover struct {
		path   string
		reason string
	}
	var left []leftover
	seenLeft := make(map[string]struct{})
	addLeft := func(p, reason string) {
		if _, ok := seenLeft[p]; ok {
			return
		}
		seenLeft[p] = struct{}{}
		left = append(left, leftover{path: p, reason: strings.ReplaceAll(reason, "\n", " ")})
	}

	// Remove socket (and empty /tmp fallback dir if used).
	if sockPath != "" {
		if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			addLeft(sockPath, err.Error())
		}
		if layout.SocketUsesTmpDir {
			_ = os.Remove(layout.SocketDir)
		}
	}

	// Freshly check whether other machines still have install.json under SharedLock.
	isLast, lastCheckErr := isLastMachineInStateRoot(layout.StateRoot, layout.MachineID)
	if lastCheckErr != nil {
		isLast = false
		addLeft(layout.StateRoot, fmt.Sprintf("unable to verify other machines (shared binaries preserved): %v", lastCheckErr))
	}

	// Rule 6: If install.json was missing or unreadable (whether last machine
	// or not), NEVER delete shared program files and ALWAYS report partial with
	// remaining paths (unless nothing was installed at all).
	var sharedTargets []string
	if installErr != nil {
		preserved := listPreservedSharedPaths(layout)
		if stateDirExists || len(preserved) > 0 {
			for _, p := range preserved {
				addLeft(p, "install.json missing or unreadable; shared file preserved")
			}
			if len(preserved) == 0 {
				addLeft(layout.InstallJSON, fmt.Sprintf("install.json missing or unreadable: %v", installErr))
			}
		}
	} else if isLast && lastCheckErr == nil && libDirExists {
		sharedTargets = collectSharedCorraldTargets(layout, installRec)
		for _, target := range sharedTargets {
			if err := os.RemoveAll(target); err != nil {
				addLeft(target, err.Error())
			}
		}
	}

	// Rule 4: Explicitly delete all non-lock entries inside StateDir, keeping
	// op.lock and corrald.lock intact (avoids unlink+new-inode lock races),
	// then remove the tombstone last (serve still refuses because install.json
	// is gone).
	if stateDirExists {
		entries, rerr := os.ReadDir(layout.StateDir)
		if rerr != nil {
			addLeft(layout.StateDir, rerr.Error())
		} else {
			for _, e := range entries {
				name := e.Name()
				if name == "op.lock" || name == "corrald.lock" || name == "removing" {
					continue
				}
				p := filepath.Join(layout.StateDir, name)
				if err := os.RemoveAll(p); err != nil {
					addLeft(p, err.Error())
				}
			}
			if err := os.Remove(layout.TombstonePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				addLeft(layout.TombstonePath, err.Error())
			}
		}
	}

	// Post-check verification (回查).
	if metaBefore != nil && ProcessMatches(metaBefore.PID, metaBefore.StartTime) {
		fmt.Fprintf(stdout, "error=stop_failed\n")
		fmt.Fprintf(stdout, "pid=%d\n", metaBefore.PID)
		return 1
	}
	if sockPath != "" {
		if _, err := os.Lstat(sockPath); err == nil {
			addLeft(sockPath, "socket still exists after removal")
		}
	}
	if stateDirExists {
		if entries, err := os.ReadDir(layout.StateDir); err == nil {
			for _, e := range entries {
				name := e.Name()
				if name == "op.lock" || name == "corrald.lock" {
					continue
				}
				addLeft(filepath.Join(layout.StateDir, name), "state entry still exists after removal")
			}
		}
	}
	if isLast && installErr == nil && installRec != nil {
		for _, target := range sharedTargets {
			if _, err := os.Lstat(target); err == nil {
				addLeft(target, "path still exists after removal")
			}
		}
	}

	if len(left) > 0 {
		fmt.Fprintf(stdout, "result=partial\n")
		fmt.Fprintf(stdout, "last_machine=%t\n", isLast)
		for _, l := range left {
			fmt.Fprintf(stdout, "left=%s\t%s\n", l.path, l.reason)
		}
		return 1
	}

	fmt.Fprintf(stdout, "result=removed\n")
	fmt.Fprintf(stdout, "last_machine=%t\n", isLast)
	return 0
}

func isLastMachineInStateRoot(stateRoot, currentMID string) (bool, error) {
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == currentMID {
			continue
		}
		_, err := os.Stat(filepath.Join(stateRoot, e.Name(), "install.json"))
		if err == nil {
			return false, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return false, err
	}
	return true, nil
}

func collectSharedCorraldTargets(layout *Layout, installRec *InstallRecord) []string {
	if installRec == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var targets []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		targets = append(targets, p)
	}

	cleanLib := filepath.Clean(layout.LibDir)
	for _, f := range installRec.Files {
		clean := filepath.Clean(f)
		if filepath.Dir(clean) != cleanLib {
			continue
		}
		base := filepath.Base(clean)
		if base == "corrald" || base == "README.txt" {
			add(clean)
			continue
		}
		if _, _, ok := ParseVersionDirName(base); ok {
			add(clean)
		}
	}
	return targets
}

func listPreservedSharedPaths(layout *Layout) []string {
	var preserved []string
	if _, err := os.Lstat(layout.CurrentSymlink); err == nil {
		preserved = append(preserved, layout.CurrentSymlink)
	}
	if _, err := os.Lstat(layout.ReadmePath); err == nil {
		preserved = append(preserved, layout.ReadmePath)
	}
	if entries, err := os.ReadDir(layout.LibDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				if _, _, ok := ParseVersionDirName(e.Name()); ok {
					preserved = append(preserved, filepath.Join(layout.LibDir, e.Name()))
				}
			}
		}
	}
	return preserved
}

func callLocalRPC(sockPath, method string, params any, dst any, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		rawParams = b
	}
	if err := proto.WriteJSON(conn, proto.Request{
		ID:     1,
		Method: method,
		Params: rawParams,
	}); err != nil {
		return err
	}

	for {
		var env proto.Envelope
		if err := proto.ReadJSON(conn, &env); err != nil {
			return err
		}
		if env.IsEvent() {
			continue
		}
		if env.ID != 1 {
			continue
		}
		if env.Error != nil {
			return env.Error
		}
		if dst != nil && len(env.Result) > 0 {
			return json.Unmarshal(env.Result, dst)
		}
		return nil
	}
}
