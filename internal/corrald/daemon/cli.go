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
	opts := defaultPathOptions()
	return ResolveLayout(opts)
}

func writeErr(w io.Writer, code, detail string) {
	fmt.Fprintf(w, "error=%s\n", code)
	if detail != "" {
		detail = strings.ReplaceAll(detail, "\n", " ")
		fmt.Fprintf(w, "detail=%s\n", detail)
	}
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

	// Test flock on a dedicated temporary lock file so we do not disturb a live daemon.
	probeLockPath := filepath.Join(layout.StateDir, fmt.Sprintf(".selftest-%d.lock", os.Getpid()))
	lock, err := AcquireLock(probeLockPath)
	if err != nil {
		writeErr(stdout, "flock_failed", err.Error())
		return 1
	}
	_ = lock.Release()
	_ = os.Remove(probeLockPath)

	// Verify socket parent directory and ability to bind a Unix domain socket.
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
	stageDir := fs.String("stage", "", "staging directory containing corrald binary")
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

	// Tombstone enforces withdrawal-first: refuse install/upgrade while removing exists.
	if layout.TombstoneExists() {
		writeErr(stdout, proto.ErrRemoving, "uninstall tombstone present")
		return 1
	}

	if err := os.MkdirAll(layout.LibDir, 0o755); err != nil {
		writeErr(stdout, "mkdir_failed", err.Error())
		return 1
	}
	if err := layout.EnsureStateDir(); err != nil {
		writeErr(stdout, "state_dir_failed", err.Error())
		return 1
	}

	// Re-check tombstone after state dir creation.
	if layout.TombstoneExists() {
		writeErr(stdout, proto.ErrRemoving, "uninstall tombstone present")
		return 1
	}

	srcPath := filepath.Join(*stageDir, "corrald")
	if info, err := os.Stat(*stageDir); err == nil && info.Mode().IsRegular() {
		srcPath = *stageDir
	}
	srcInfo, err := os.Lstat(srcPath)
	if err != nil || !srcInfo.Mode().IsRegular() {
		writeErr(stdout, "stage_missing", fmt.Sprintf("staged binary not found at %s", srcPath))
		return 1
	}

	verDir, err := layout.VersionDir(build.Version, build.Release)
	if err != nil {
		writeErr(stdout, "invalid_version", err.Error())
		return 1
	}
	if err := os.MkdirAll(verDir, 0o700); err != nil {
		writeErr(stdout, "mkdir_failed", err.Error())
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

	// Remove staging directory after installation succeeds.
	if err := os.RemoveAll(*stageDir); err != nil {
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
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	tmp := filepath.Join(dir, fmt.Sprintf(".corrald.tmp.%d.%s", os.Getpid(), RandomHex(4)))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
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
	if err := fs.Parse(args); err != nil {
		writeErr(stdout, "invalid_args", err.Error())
		return 2
	}

	layout, err := resolveDefaultLayout()
	if err != nil {
		writeErr(stdout, "layout_failed", err.Error())
		return 1
	}

	// Tombstone check: never start if uninstall is in progress.
	if layout.TombstoneExists() {
		writeErr(stdout, proto.ErrRemoving, "uninstall tombstone present")
		return 1
	}

	if err := layout.EnsureStateDir(); err != nil {
		writeErr(stdout, "state_dir_failed", err.Error())
		return 1
	}

	// Self-detach when not running in foreground.
	if !*foreground {
		return serveDetached(layout, *gateway, stdout)
	}

	return serveForeground(layout, build, *gateway, *handoff, *listenFD, *lockFD, *stateFD, *readyFD, stdout)
}

func serveDetached(layout *Layout, gateway string, stdout io.Writer) int {
	// Quick lock probe: if already running, report running without spawning.
	probeLock, err := AcquireLock(layout.LockPath)
	if errors.Is(err, ErrLocked) {
		pid, sock := waitForRunningMeta(layout, 2*time.Second)
		fmt.Fprintf(stdout, "result=running\n")
		fmt.Fprintf(stdout, "pid=%d\n", pid)
		fmt.Fprintf(stdout, "socket=%s\n", sock)
		return 0
	}
	if err != nil {
		writeErr(stdout, "lock_failed", err.Error())
		return 1
	}
	// Release probe lock before child acquires it.
	_ = probeLock.Release()

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
	logFile, err := os.OpenFile(layout.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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

	// Wait up to 5 seconds for child readiness report.
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

	// Optional test-only hook to simulate a broken replacement binary that exits
	// before sending READY.
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

	var lock *FileLock
	if handoff && lockFD >= 0 {
		lock, err = AdoptLock(uintptr(lockFD))
		if err != nil {
			return reportReadyErr("adopt_lock_failed", err.Error())
		}
	} else {
		lock, err = AcquireLock(layout.LockPath)
		if errors.Is(err, ErrLocked) {
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
	}
	defer lock.Release()

	var (
		ln   *net.UnixListener
		snap *HandoffSnapshot
	)
	if handoff {
		if stateFD >= 0 {
			sf := os.NewFile(uintptr(stateFD), "state-pipe")
			rawSnap, rerr := io.ReadAll(io.LimitReader(sf, 16<<20))
			_ = sf.Close()
			if rerr != nil {
				logger.Warn("failed reading handoff state pipe; continuing with fresh epoch", "err", rerr)
			} else if len(rawSnap) > 0 {
				var parsed HandoffSnapshot
				if jerr := json.Unmarshal(rawSnap, &parsed); jerr != nil {
					logger.Warn("corrupt handoff snapshot; continuing with fresh epoch", "err", jerr)
				} else {
					snap = &parsed
					if snap.SocketPath != "" {
						layout.SocketPath = snap.SocketPath
						layout.SocketDir = filepath.Dir(snap.SocketPath)
					}
					if gateway == "" && snap.Gateway != "" {
						gateway = snap.Gateway
					}
				}
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
		Logger:          logger,
		Lock:            lock,
		Listener:        ln,
		InitialSnapshot: snap,
	}
	applyTestServerConfig(&cfg)

	cfg.OnReady = func() {
		// On hot-update handoff, atomically switch the current symlink and update
		// install.json + README.txt right before signaling READY (spec §6 item 4).
		if handoff {
			finalizeHandoffCutover(layout, build, logger)
		}
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

func finalizeHandoffCutover(layout *Layout, build proto.BuildInfo, log *slog.Logger) {
	exe, err := os.Executable()
	if err != nil {
		log.Warn("handoff cutover: resolve executable failed", "err", err)
		return
	}
	realExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		realExe = exe
	}
	realLib, err := filepath.EvalSymlinks(layout.LibDir)
	if err != nil {
		realLib = layout.LibDir
	}
	rel, err := filepath.Rel(realLib, realExe)
	if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		if err := AtomicSymlink(rel, layout.CurrentSymlink); err != nil {
			log.Warn("handoff cutover: atomic symlink failed", "err", err)
		}
	}
	if rec, err := ReadInstallRecord(layout.InstallJSON); err == nil {
		rec.Version = build.Version
		rec.Release = build.Release
		rec.Commit = build.Commit
		rec.Files = append(rec.Files, filepath.Dir(realExe), realExe)
		if err := WriteInstallRecord(layout.InstallJSON, rec); err != nil {
			log.Warn("handoff cutover: update install.json failed", "err", err)
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
		_ = WriteFileAtomic(layout.ReadmePath, []byte(readmeText), 0o644)
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

	// Query live daemon over Unix socket via daemon.health first.
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

	// Fallback: inspect meta.json and /proc/<pid>/stat.
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

// StopDaemon stops the running corrald instance for layout: first requesting
// graceful shutdown over the Unix socket, then falling back to SIGTERM (3 s)
// and SIGKILL only when both pid and /proc/<pid>/stat field-22 start time match
// meta.json.
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

	// 1. Try graceful RPC shutdown via Unix socket.
	var res proto.ShutdownResult
	rpcOK := callLocalRPC(sockPath, proto.MethodDaemonShutdown, proto.ShutdownParams{Reason: "stop"}, &res, 2*time.Second) == nil
	if rpcOK {
		wasRunning = true
		if waitProcessGone(layout, meta, 3*time.Second) {
			cleanupStoppedArtifacts(layout, sockPath)
			return true, pid, nil
		}
	}

	// Re-read meta.json in case a handoff occurred right before stop.
	if freshMeta, err := ReadMetaRecord(layout.MetaJSON); err == nil {
		meta = freshMeta
		metaErr = nil
		pid = meta.PID
		if meta.SocketPath != "" {
			sockPath = meta.SocketPath
		}
	}

	if metaErr != nil || !ProcessMatches(meta.PID, meta.StartTime) {
		cleanupStoppedArtifacts(layout, sockPath)
		return wasRunning, 0, nil
	}

	// 2. Send SIGTERM only when PID and /proc/<pid>/stat start time match.
	_ = syscall.Kill(meta.PID, syscall.SIGTERM)
	if waitProcessGone(layout, meta, 3*time.Second) {
		cleanupStoppedArtifacts(layout, sockPath)
		return true, meta.PID, nil
	}

	// 3. Re-verify identity before SIGKILL.
	if !ProcessMatches(meta.PID, meta.StartTime) {
		cleanupStoppedArtifacts(layout, sockPath)
		return true, meta.PID, nil
	}
	_ = syscall.Kill(meta.PID, syscall.SIGKILL)
	if waitProcessGone(layout, meta, 2*time.Second) {
		cleanupStoppedArtifacts(layout, sockPath)
		return true, meta.PID, nil
	}
	return false, meta.PID, fmt.Errorf("process %d did not exit after SIGTERM and SIGKILL", meta.PID)
}

func waitProcessGone(layout *Layout, meta *MetaRecord, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if meta != nil && meta.PID > 0 && meta.StartTime != "" {
			if !ProcessMatches(meta.PID, meta.StartTime) {
				return true
			}
		} else {
			// Without meta, check if flock is free.
			if lock, err := AcquireLock(layout.LockPath); err == nil {
				_ = lock.Release()
				return true
			}
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

	// 1. Write removing tombstone immediately if StateDir exists (or can be created).
	if _, err := os.Stat(layout.StateDir); err == nil {
		_ = WriteFileAtomic(layout.TombstonePath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}

	// Capture meta & install records before stopping/deleting.
	metaBefore, _ := ReadMetaRecord(layout.MetaJSON)
	installRec, _ := ReadInstallRecord(layout.InstallJSON)
	sockPath := layout.SocketPath
	if metaBefore != nil && metaBefore.SocketPath != "" {
		sockPath = metaBefore.SocketPath
	}

	// 2. Stop running daemon. If stop fails, do not delete files (spec §4 & design doc).
	_, pid, stopErr := StopDaemon(layout)
	if stopErr != nil {
		fmt.Fprintf(stdout, "error=stop_failed\n")
		fmt.Fprintf(stdout, "pid=%d\n", pid)
		fmt.Fprintf(stdout, "detail=%s\n", strings.ReplaceAll(stopErr.Error(), "\n", " "))
		return 1
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

	// 3. Remove socket (and empty /tmp fallback dir if used).
	if sockPath != "" {
		if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			addLeft(sockPath, err.Error())
		}
		if layout.SocketUsesTmpDir {
			_ = os.Remove(layout.SocketDir)
		}
	}

	// 4. Determine whether this machine is the last machine in a shared HOME (NFS).
	isLast := isLastMachineInStateRoot(layout.StateRoot, layout.MachineID)

	// 5. Delete this machine's state directory.
	if err := os.RemoveAll(layout.StateDir); err != nil {
		addLeft(layout.StateDir, err.Error())
	}

	// 6. Only if this is the last machine, remove shared corrald binaries,
	// symlink, and README.txt — NEVER touching code-server or other entries.
	var sharedTargets []string
	if isLast {
		sharedTargets = collectSharedCorraldTargets(layout, installRec)
		for _, target := range sharedTargets {
			if err := os.RemoveAll(target); err != nil {
				addLeft(target, err.Error())
			}
		}
		// Remove StateRoot / LibDir only if completely empty.
		_ = os.Remove(layout.StateRoot)
		_ = os.Remove(layout.LibDir)
	}

	// 7. Post-check verification (回查): process exited, socket gone, state dir gone,
	// and shared targets gone (when last machine).
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
	if _, err := os.Lstat(layout.StateDir); err == nil {
		addLeft(layout.StateDir, "state directory still exists after removal")
	}
	if isLast {
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

func isLastMachineInStateRoot(stateRoot, currentMID string) bool {
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == currentMID {
			continue
		}
		if _, err := os.Stat(filepath.Join(stateRoot, e.Name(), "install.json")); err == nil {
			return false
		}
	}
	return true
}

func collectSharedCorraldTargets(layout *Layout, installRec *InstallRecord) []string {
	seen := make(map[string]struct{})
	var targets []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		targets = append(targets, p)
	}

	add(layout.CurrentSymlink)
	add(layout.ReadmePath)

	// Any corrald-* version directories (or stray .corrald.tmp.*) under LibDir.
	if entries, err := os.ReadDir(layout.LibDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "corrald-") || strings.HasPrefix(name, ".corrald.tmp.") {
				add(filepath.Join(layout.LibDir, name))
			}
		}
	}

	// Also include any corrald-* directories recorded in install.json strictly inside LibDir.
	if installRec != nil {
		for _, f := range installRec.Files {
			clean := filepath.Clean(f)
			if filepath.Dir(clean) == layout.LibDir {
				base := filepath.Base(clean)
				if base == "corrald" || base == "README.txt" || strings.HasPrefix(base, "corrald-") {
					add(clean)
				}
			}
		}
	}
	return targets
}

// callLocalRPC connects to sockPath, invokes method with params, and decodes
// the result into dst.
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
