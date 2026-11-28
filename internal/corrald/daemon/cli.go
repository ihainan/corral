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
	"log/slog"
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
	SetProcessName("corrald")
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

	lockTimeout := defaultLifecycleLockTimeout()
	sharedLock, err := AcquireDirLockWait(layout.EnsureLibDir, layout.SharedLockPath, lockTimeout)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return writeLockErr(stdout, err)
		}
		writeErr(stdout, "lib_dir_failed", err.Error())
		return 1
	}
	defer sharedLock.Release()

	opLock, err := AcquireDirLockWait(layout.EnsureStateDir, layout.OpLockPath, lockTimeout)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return writeLockErr(stdout, err)
		}
		writeErr(stdout, "state_dir_failed", err.Error())
		return 1
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

	baseFiles := []string{
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

	// B-2: Always read any existing install.json and preserve its Files list so
	// cold-start updates (install without --upgrade) never drop previously
	// installed version directories from the uninstall manifest.
	prevRec, _ := ReadInstallRecord(layout.InstallJSON)

	var installRec *InstallRecord
	var (
		readmeVer   string
		readmeRel   int64
		forceReadme bool
	)
	if *upgrade {
		installRec = prevRec
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
		installRec.Files = append(installRec.Files, baseFiles...)
		if err := WriteInstallRecord(layout.InstallJSON, installRec); err != nil {
			writeErr(stdout, "install_json_failed", err.Error())
			return 1
		}
		curVer, curRel, valid, serr := inspectSharedSymlink(layout)
		if serr != nil {
			writeErr(stdout, "symlink_failed", serr.Error())
			return 1
		}
		if valid {
			readmeVer, readmeRel = curVer, curRel
		} else {
			readmeVer, readmeRel = installRec.Version, installRec.Release
		}
	} else {
		updateLink, curVer, curRel, serr := shouldUpdateSharedSymlink(layout, build.Release)
		if serr != nil {
			writeErr(stdout, "symlink_failed", serr.Error())
			return 1
		}
		if updateLink {
			relTarget, err := filepath.Rel(layout.LibDir, dstBin)
			if err != nil {
				writeErr(stdout, "symlink_failed", err.Error())
				return 1
			}
			if err := AtomicSymlink(relTarget, layout.CurrentSymlink); err != nil {
				writeErr(stdout, "symlink_failed", err.Error())
				return 1
			}
			readmeVer, readmeRel = build.Version, build.Release
			forceReadme = true
		} else {
			readmeVer, readmeRel = curVer, curRel
		}
		files := make([]string, 0, len(baseFiles))
		if prevRec != nil && len(prevRec.Files) > 0 {
			files = append(files, prevRec.Files...)
		}
		files = append(files, baseFiles...)
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

	if err := ensureSharedReadme(layout, installRec, readmeVer, readmeRel, forceReadme); err != nil {
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
	tmuxPath := fs.String("tmux", "", "optional absolute path to tmux binary")
	foreground := fs.Bool("foreground", false, "run in foreground without detaching")
	handoff := fs.Bool("handoff", false, "internal: adopt inherited fds during hot update")
	listenFD := fs.Int("listen-fd", -1, "internal: inherited Unix listener fd")
	lockFD := fs.Int("lock-fd", -1, "internal: inherited flock fd")
	sharedLockFD := fs.Int("shared-lock-fd", -1, "internal: inherited shared lifecycle flock fd")
	opLockFD := fs.Int("op-lock-fd", -1, "internal: inherited host op.lock flock fd")
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

	validatedTmux := ""
	if *tmuxPath != "" {
		var terr error
		validatedTmux, terr = ValidateTmuxBinary(*tmuxPath, layout.UID)
		if terr != nil {
			// C-9: optional --tmux failure must never block serve or handoff;
			// log and continue as if --tmux was not provided.
			slog.Warn("ignoring invalid --tmux path", "path", *tmuxPath, "err", terr)
			validatedTmux = ""
		}
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
		return serveDetached(layout, *gateway, validatedTmux, stdout)
	}

	return serveForeground(layout, build, *gateway, validatedTmux, *handoff, *listenFD, *lockFD, *sharedLockFD, *opLockFD, *stateFD, *readyFD, *binaryPath, stdout)
}

func serveDetached(layout *Layout, gateway, tmuxPath string, stdout io.Writer) int {
	opLock, err := AcquireLockWait(layout.OpLockPath, defaultLifecycleLockTimeout())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(stdout, "not_installed", "install.json not found")
			return 1
		}
		return writeLockErr(stdout, err)
	}
	if code, detail, ok := checkServeAllowed(layout); !ok {
		_ = opLock.Release()
		writeErr(stdout, code, detail)
		return 1
	}
	probeLock, runPID, runSock, busy, err := acquireServeLockOrConfirmRunning(layout, 3*time.Second)
	if err != nil {
		_ = opLock.Release()
		if errors.Is(err, os.ErrNotExist) {
			writeErr(stdout, "not_installed", "install.json not found")
			return 1
		}
		writeErr(stdout, "lock_failed", err.Error())
		return 1
	}
	if busy {
		_ = opLock.Release()
		writeErr(stdout, proto.ErrBusy, "existing daemon is stopping or busy")
		return 1
	}
	if probeLock == nil {
		_ = opLock.Release()
		fmt.Fprintf(stdout, "result=running\n")
		fmt.Fprintf(stdout, "pid=%d\n", runPID)
		fmt.Fprintf(stdout, "socket=%s\n", runSock)
		return 0
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
	if tmuxPath != "" {
		argv = append(argv, "--tmux", tmuxPath)
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
	tmuxPath string,
	handoff bool,
	listenFD, lockFD, sharedLockFD, opLockFD, stateFD, readyFD int,
	binaryPath string,
	stdout io.Writer,
) int {
	var (
		readyFile  *os.File
		lock       *FileLock
		sharedLock *FileLock
		opLock     *FileLock
	)
	if readyFD >= 0 {
		readyFile = os.NewFile(uintptr(readyFD), "ready-pipe")
	}
	releaseLifecycleLocks := func() {
		if opLock != nil {
			_ = opLock.Release()
			opLock = nil
		}
		if sharedLock != nil {
			_ = sharedLock.Release()
			sharedLock = nil
		}
	}
	defer releaseLifecycleLocks()

	reportReadyErr := func(code, detail string) int {
		if readyFile != nil {
			_, _ = fmt.Fprintf(readyFile, "ERROR %s %s\n", code, strings.ReplaceAll(detail, "\n", " "))
			_ = readyFile.Close()
			readyFile = nil
		}
		writeErr(stdout, code, detail)
		return 1
	}

	// Adopt inherited lifecycle lock FDs immediately so they remain held even
	// if parent v1 is SIGKILL'd before READY, and are closed cleanly on any
	// error return or once OnReady completes cutover.
	if handoff {
		if sharedLockFD >= 0 {
			var err error
			sharedLock, err = AdoptLock(uintptr(sharedLockFD))
			if err != nil {
				return reportReadyErr("adopt_lock_failed", err.Error())
			}
		}
		if opLockFD >= 0 {
			var err error
			opLock, err = AdoptLock(uintptr(opLockFD))
			if err != nil {
				return reportReadyErr("adopt_lock_failed", err.Error())
			}
		}
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

	if handoff {
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
			if errors.Is(err, os.ErrNotExist) {
				return reportReadyErr("not_installed", "install.json not found")
			}
			if errors.Is(err, ErrLocked) {
				return reportReadyErr(proto.ErrBusy, err.Error())
			}
			return reportReadyErr("lock_failed", err.Error())
		}
		if code, detail, ok := checkServeAllowed(layout); !ok {
			return reportReadyErr(code, detail)
		}
		var (
			runPID  int
			runSock string
			busy    bool
		)
		lock, runPID, runSock, busy, err = acquireServeLockOrConfirmRunning(layout, 3*time.Second)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return reportReadyErr("not_installed", "install.json not found")
			}
			return reportReadyErr("lock_failed", err.Error())
		}
		if busy {
			return reportReadyErr(proto.ErrBusy, "existing daemon is stopping or busy")
		}
		if lock == nil {
			releaseLifecycleLocks()
			if readyFile != nil {
				_, _ = fmt.Fprintf(readyFile, "RUNNING %d %s\n", runPID, runSock)
				_ = readyFile.Close()
				return 0
			}
			fmt.Fprintf(stdout, "result=running\n")
			fmt.Fprintf(stdout, "pid=%d\n", runPID)
			fmt.Fprintf(stdout, "socket=%s\n", runSock)
			return 0
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
		if sharedLock == nil || opLock == nil {
			return reportReadyErr("missing_lifecycle_locks", "handoff requires inherited shared-lock-fd and op-lock-fd")
		}
		snap = &parsed
		if snap.SocketPath != "" {
			layout.SocketPath = snap.SocketPath
			layout.SocketDir = filepath.Dir(snap.SocketPath)
		}
		if gateway == "" && snap.Gateway != "" {
			gateway = snap.Gateway
		}
		if tmuxPath == "" && snap.TmuxPath != "" {
			if validTmux, terr := ValidateTmuxBinary(snap.TmuxPath, layout.UID); terr == nil {
				tmuxPath = validTmux
			} else {
				slog.Warn("ignoring invalid tmux path from handoff snapshot", "path", snap.TmuxPath, "err", terr)
			}
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
		TmuxPath:        tmuxPath,
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
		releaseLifecycleLocks()
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
	attachRegisteredModules(srv)

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

// inspectSharedSymlink validates layout.CurrentSymlink and checks that its
// target binary exists inside a valid corrald-<version>-<release> directory
// under layout.LibDir and passes the same checks as installation (non-symlink
// regular file, owned by layout.UID, mode 0700 inside a 0700 directory owned
// by layout.UID). Returns valid=false when the symlink is absent or its target
// is missing/invalid; returns an error on unexpected filesystem read errors.
func inspectSharedSymlink(layout *Layout) (curVersion string, curRelease int64, valid bool, err error) {
	target, err := os.Readlink(layout.CurrentSymlink)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("readlink %s: %w", layout.CurrentSymlink, err)
	}
	targetPath := target
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(layout.LibDir, targetPath)
	}
	targetPath = filepath.Clean(targetPath)
	parentDir := filepath.Dir(targetPath)
	if filepath.Dir(parentDir) != filepath.Clean(layout.LibDir) || filepath.Base(targetPath) != "corrald" {
		return "", 0, false, nil
	}
	ver, rel, ok := ParseVersionDirName(filepath.Base(parentDir))
	if !ok {
		return "", 0, false, nil
	}

	pinfo, err := os.Lstat(parentDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("lstat symlink target dir %s: %w", parentDir, err)
	}
	if pinfo.Mode()&os.ModeSymlink != 0 || !pinfo.IsDir() {
		return "", 0, false, nil
	}
	if pst, ok := pinfo.Sys().(*syscall.Stat_t); !ok || pst.Uid != layout.UID || pinfo.Mode().Perm() != 0o700 {
		return "", 0, false, nil
	}

	tinfo, err := os.Lstat(targetPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("lstat symlink target %s: %w", targetPath, err)
	}
	if tinfo.Mode()&os.ModeSymlink != 0 || !tinfo.Mode().IsRegular() {
		return "", 0, false, nil
	}
	if tst, ok := tinfo.Sys().(*syscall.Stat_t); !ok || tst.Uid != layout.UID || tinfo.Mode().Perm() != 0o700 {
		return "", 0, false, nil
	}
	return ver, rel, true, nil
}

// shouldUpdateSharedSymlink reports whether layout.CurrentSymlink should be
// switched to newRelease. It returns true when the symlink does not exist, its
// target binary fails validation, or its valid target has a strictly lower
// Release. When returning false, curVersion/curRelease identify the valid
// target currently pointed to by CurrentSymlink.
func shouldUpdateSharedSymlink(layout *Layout, newRelease int64) (update bool, curVersion string, curRelease int64, err error) {
	curVersion, curRelease, valid, err := inspectSharedSymlink(layout)
	if err != nil {
		return false, "", 0, err
	}
	if !valid {
		return true, "", 0, nil
	}
	return proto.ShouldUpgrade(curRelease, newRelease), curVersion, curRelease, nil
}

// readmeMatchesVersion reports whether readmePath exists as a regular
// non-symlink file describing the given (version, release).
func readmeMatchesVersion(readmePath, version string, release int64) (bool, error) {
	info, err := os.Lstat(readmePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("lstat %s: %w", readmePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil
	}
	b, err := os.ReadFile(readmePath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", readmePath, err)
	}
	ver := strings.TrimSpace(version)
	if ver == "" {
		ver = "dev"
	}
	zhLine := fmt.Sprintf("  版本：%s（release %d）\n", ver, release)
	enLine := fmt.Sprintf("  Version:    %s (release %d)\n", ver, release)
	text := string(b)
	return strings.Contains(text, zhLine) && strings.Contains(text, enLine), nil
}

// ensureSharedReadme ensures layout.ReadmePath exists and describes
// (version, release). Must be called while holding SharedLock.
func ensureSharedReadme(layout *Layout, rec *InstallRecord, version string, release int64, force bool) error {
	if !force {
		matches, err := readmeMatchesVersion(layout.ReadmePath, version, release)
		if err != nil {
			return err
		}
		if matches {
			return nil
		}
	}
	data := proto.ReadmeData{
		Version:    version,
		Release:    release,
		MachineID:  layout.MachineID,
		LibDir:     layout.LibDir,
		StateDir:   layout.StateDir,
		SocketPath: layout.SocketPath,
	}
	if rec != nil {
		data.Gateway = rec.Gateway
		data.EnabledBy = rec.EnabledBy
		data.EnabledAt = rec.EnabledAt
		data.Files = rec.Files
	}
	readmeText := proto.RenderReadme(data)
	return WriteFileAtomic(layout.ReadmePath, []byte(readmeText), 0o644)
}

// finalizeHandoffCutover executes inside the inherited SharedLock -> OpLock
// critical section. It verifies the target binary exists on disk and updates
// symlink (only if current shared link is absent, invalid, or lower release),
// ensures README.txt matches the active shared link version, and updates local
// install.json and meta.json BEFORE Accept starts and BEFORE READY is signaled.
func finalizeHandoffCutover(layout *Layout, build proto.BuildInfo, binaryPath string, meta *MetaRecord) error {
	waitCutoverTestGate()

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

	updateLink, curVer, curRel, err := shouldUpdateSharedSymlink(layout, build.Release)
	if err != nil {
		return fmt.Errorf("check shared symlink: %w", err)
	}
	readmeVer, readmeRel := curVer, curRel
	if updateLink {
		if err := AtomicSymlink(rel, layout.CurrentSymlink); err != nil {
			return fmt.Errorf("switch symlink: %w", err)
		}
		readmeVer, readmeRel = build.Version, build.Release
	}

	var rec *InstallRecord
	if loaded, err := ReadInstallRecord(layout.InstallJSON); err == nil {
		rec = loaded
		rec.Version = build.Version
		rec.Release = build.Release
		rec.Commit = build.Commit
		rec.Files = append(rec.Files, filepath.Dir(realExe), realExe)
		if err := WriteInstallRecord(layout.InstallJSON, rec); err != nil {
			return fmt.Errorf("update install.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read install.json: %w", err)
	}

	if err := ensureSharedReadme(layout, rec, readmeVer, readmeRel, updateLink); err != nil {
		return fmt.Errorf("update README.txt: %w", err)
	}

	if err := WriteMetaRecord(layout.MetaJSON, meta); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}
	return nil
}

func acquireServeLockOrConfirmRunning(layout *Layout, wait time.Duration) (lock *FileLock, pid int, sock string, busy bool, err error) {
	lock, err = AcquireLock(layout.LockPath)
	if err == nil {
		return lock, 0, "", false, nil
	}
	if !errors.Is(err, ErrLocked) {
		return nil, 0, "", false, err
	}

	deadline := time.Now().Add(wait)
	for {
		candSock := layout.SocketPath
		candPID := 0
		if meta, merr := ReadMetaRecord(layout.MetaJSON); merr == nil {
			if meta.PID > 0 {
				candPID = meta.PID
			}
			if meta.SocketPath != "" {
				candSock = meta.SocketPath
			}
		}
		var health proto.HealthResult
		if herr := callLocalRPC(candSock, proto.MethodDaemonHealth, nil, &health, 300*time.Millisecond); herr == nil {
			if health.State == "" || health.State == proto.StateRunning {
				if health.PID > 0 {
					candPID = health.PID
				}
				if health.Socket != "" {
					candSock = health.Socket
				}
				return nil, candPID, candSock, false, nil
			}
		}

		lock, err = AcquireLock(layout.LockPath)
		if err == nil {
			return lock, 0, "", false, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, 0, "", false, err
		}
		if time.Now().After(deadline) {
			return nil, 0, "", true, nil
		}
		time.Sleep(40 * time.Millisecond)
	}
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
		sharedLock, err = AcquireLockWait(layout.SharedLockPath, lockTimeout)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Concurrent uninstall already removed LibDir.
				libDirExists = false
			} else {
				return writeLockErr(stdout, err)
			}
		} else {
			libDirExists = true
			defer sharedLock.Release()
		}
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
			if errors.Is(err, os.ErrNotExist) {
				stateDirExists = false
				installErr = os.ErrNotExist
			} else {
				return writeLockErr(stdout, err)
			}
		} else {
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

	// Freshly check whether other machines still have install.json under SharedLock.
	isLast, lastCheckErr := isLastMachineInStateRoot(layout.StateRoot, layout.MachineID)
	if lastCheckErr != nil {
		isLast = false
		addLeft(layout.StateRoot, fmt.Sprintf("unable to verify other machines (shared binaries preserved): %v", lastCheckErr))
	}

	// Before deleting binaries / state dir on the last machine, acquire every
	// existing pane send lock in ~/.local/state/corral/run (in the same fixed
	// order as senders) and remove run/ under lock. If any SSH send still holds
	// a lock there, restore install.json while keeping the removing tombstone
	// (so serve remains blocked and retry still has the full manifest), and
	// abort with error=busy so gateway retries cleanly.
	willPreserveForMissingInstall := installErr != nil && (stateDirExists || len(listPreservedSharedPaths(layout)) > 0)
	if isLast && lastCheckErr == nil && !willPreserveForMissingInstall && layout.RunDir != "" {
		if rerr := RemoveRunDirLocked(layout.RunDir, defaultUninstallSystemRunDir(layout.UID), layout.UID, defaultRunDirLockTimeout()); rerr != nil {
			if errors.Is(rerr, ErrLocked) {
				if stateDirExists && installErr == nil && installRec != nil {
					_ = WriteInstallRecord(layout.InstallJSON, installRec)
				}
				writeErr(stdout, proto.ErrBusy, "pane send lock is held in run dir")
				return 1
			}
			addLeft(layout.RunDir, rerr.Error())
		}
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
		// B-2: Verify no corrald-owned files/version dirs remain in LibDir
		// (even if omitted from an older install.json) before declaring removed.
		if libDirExists {
			for _, rem := range listPreservedSharedPaths(layout) {
				addLeft(rem, "corrald file still exists in lib dir after removal")
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

	// Part B: Removal succeeded with zero leftovers. While still holding
	// lifeLock -> opLock -> sharedLock, unlink the lock files and remove any
	// now-empty Corral-created directories (os.Remove only removes empty dirs).
	// All lock acquirers verify path inode == locked fd inode after flock, so
	// unlinking while held cannot create two mutually non-exclusive locks.
	// B-2: Do NOT ignore non-empty removal failures on corrald-owned dirs.
	if stateDirExists {
		if err := os.Remove(layout.LockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			addLeft(layout.LockPath, err.Error())
		}
		if err := os.Remove(layout.OpLockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			addLeft(layout.OpLockPath, err.Error())
		}
		if err := os.Remove(layout.StateDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			if entries, rerr := os.ReadDir(layout.StateDir); rerr == nil && len(entries) > 0 {
				for _, e := range entries {
					addLeft(filepath.Join(layout.StateDir, e.Name()), "file still exists in state dir after removal")
				}
			} else {
				addLeft(layout.StateDir, err.Error())
			}
		}
		if err := os.Remove(layout.StateRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
			if isLast && lastCheckErr == nil {
				if entries, rerr := os.ReadDir(layout.StateRoot); rerr == nil && len(entries) > 0 {
					for _, e := range entries {
						addLeft(filepath.Join(layout.StateRoot, e.Name()), "leftover entry in corrald state root")
					}
				} else {
					addLeft(layout.StateRoot, err.Error())
				}
			}
		}
		_ = os.Remove(filepath.Dir(layout.StateRoot)) // ~/.local/state/corral (best-effort if empty)
	}
	if isLast && lastCheckErr == nil && layout.RunDir != "" {
		// RunDir was already removed under pane locks above; best-effort clean
		// its parent ~/.local/state/corral if now empty.
		_ = os.Remove(filepath.Dir(layout.StateRoot)) // ~/.local/state/corral (best-effort if empty)
	}
	if isLast && lastCheckErr == nil && installErr == nil && libDirExists {
		if err := os.Remove(layout.SharedLockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			addLeft(layout.SharedLockPath, err.Error())
		}
		if err := os.Remove(layout.LibDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			// ~/.local/lib/corral may legitimately hold sibling tools (e.g.
			// code-server-*) or user directories (corrald-backups). Report
			// partial if any corrald-owned entry remains OR if rmdir failed
			// for a reason other than non-empty sibling tools.
			corraldLeft := listPreservedSharedPaths(layout)
			for _, p := range corraldLeft {
				addLeft(p, "corrald file still exists in lib dir after removal")
			}
			if len(corraldLeft) == 0 && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
				addLeft(layout.LibDir, err.Error())
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
