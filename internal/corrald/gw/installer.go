package gw

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/sshx"
)

// ErrOpCancelled is returned internally when an install op was cancelled
// before starting the daemon.
var ErrOpCancelled = errors.New("operation cancelled")

// OpFailure carries structured failure details matching consent-design.md
// "失败处理" and web/src/corraldLogic.ts CorraldError.
type OpFailure struct {
	Code            string     `json:"code"`
	Message         string     `json:"message,omitempty"`
	Detail          string     `json:"detail,omitempty"`
	TargetVersion   string     `json:"target_version,omitempty"`
	FallbackVersion string     `json:"fallback_version,omitempty"`
	MachineID       string     `json:"machine_id,omitempty"`
	OS              string     `json:"os,omitempty"`
	Arch            string     `json:"arch,omitempty"`
	Left            []LeftItem `json:"left,omitempty"`
	PID             int        `json:"pid,omitempty"`
	KeepInstalled   bool       `json:"-"`
}

func (f *OpFailure) Error() string {
	if f == nil {
		return ""
	}
	if f.Detail != "" {
		return f.Code + ": " + f.Detail
	}
	return f.Code
}

// HostStateFiles holds parsed JSON files read from ~/.local/state/corral/corrald/<mid>/.
type HostStateFiles struct {
	Install       *HostInstallRecord
	Meta          *HostMetaRecord
	Failed        *HostFailedRecord
	Op            *HostOpRecord
	Clients       []proto.ClientInfo
	ClientsLoaded bool // true when clients.json was read or confirmed absent
}

// HostInstallRecord mirrors daemon.InstallRecord without importing linux-only daemon code.
type HostInstallRecord struct {
	Version         string   `json:"version"`
	Release         int64    `json:"release"`
	Commit          string   `json:"commit,omitempty"`
	Gateway         string   `json:"gateway"`
	EnabledBy       string   `json:"enabled_by"`
	EnabledAt       string   `json:"enabled_at"`
	MachineID       string   `json:"machine_id"`
	MachineIDSource string   `json:"machine_id_source"`
	Files           []string `json:"files"`
}

// HostMetaRecord mirrors daemon.MetaRecord.
type HostMetaRecord struct {
	PID           int    `json:"pid"`
	StartTime     string `json:"start_time"`
	PrevPID       int    `json:"prev_pid,omitempty"`
	PrevStartTime string `json:"prev_start_time,omitempty"`
	Version       string `json:"version"`
	Release       int64  `json:"release"`
	Commit        string `json:"commit,omitempty"`
	Epoch         string `json:"epoch"`
	StartedAt     string `json:"started_at"`
	SocketPath    string `json:"socket_path"`
	Gateway       string `json:"gateway,omitempty"`
}

// HostOpRecord mirrors daemon.OpRecordFile.
type HostOpRecord struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
	Release int64  `json:"release,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	By      string `json:"by,omitempty"`
	At      string `json:"at"`
	Result  string `json:"result"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// HostFailedEntry mirrors daemon.FailedEntry.
type HostFailedEntry struct {
	Release int64  `json:"release"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
}

// HostFailedRecord mirrors daemon.FailedRecord.
type HostFailedRecord struct {
	Releases []HostFailedEntry `json:"releases"`
}

// HostClientsFile parses clients.json written by daemon.ClientRegistry.
// Current daemons persist a JSON array ([]proto.ClientInfo); wrapped objects
// ({"clients":[...]}) are also accepted for backward compatibility.
type HostClientsFile struct {
	Clients []proto.ClientInfo `json:"clients"`
}

func (f *HostClientsFile) UnmarshalJSON(data []byte) error {
	var list []proto.ClientInfo
	if err := json.Unmarshal(data, &list); err == nil {
		f.Clients = list
		return nil
	}
	var wrapped struct {
		Clients []proto.ClientInfo `json:"clients"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return err
	}
	f.Clients = wrapped.Clients
	return nil
}

func (f *HostFailedRecord) ReleaseList() []int64 {
	if f == nil || len(f.Releases) == 0 {
		return nil
	}
	out := make([]int64, 0, len(f.Releases))
	for _, r := range f.Releases {
		out = append(out, r.Release)
	}
	return out
}

// UninstallOutcome holds the structured outcome of running corrald uninstall.
type UninstallOutcome struct {
	Result      string     // "removed", "partial", "stop_failed", "pending"
	Left        []LeftItem // populated on "partial"
	PID         int        // populated on "stop_failed"
	Detail      string
	LastMachine bool
}

// ProbeHost runs ProbeCommand (and ProbeStateCommand once mid is resolved)
// over sshClient.
func ProbeHost(ctx context.Context, c *ssh.Client) (*HostProbe, error) {
	res, err := runSSH(ctx, c, ProbeCommand(), sshx.MaxOutput, sshx.ExecOptions{})
	if err != nil && !res.Ended {
		return nil, err
	}
	p, perr := ParseProbe(res.Out)
	if perr != nil {
		if err != nil {
			return nil, err
		}
		return nil, perr
	}
	if p.StateDir != "" {
		sres, serr := runSSH(ctx, c, ProbeStateCommand(p.StateDir), sshx.MaxOutput, sshx.ExecOptions{})
		if serr == nil || sres.Ended {
			sf := ParseKeyValues(sres.Out)
			p.HasTombstone = sf["tombstone"] == "1"
			p.HasInstall = sf["has_install"] == "1"
			sc := bufio.NewScanner(strings.NewReader(string(sres.Out)))
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "stage_dir="); ok && v != "" {
					p.Stages = append(p.Stages, p.StateDir+"/"+v)
				}
			}
		}
	}
	return p, nil
}

// ReadHostStateFiles reads install.json, meta.json, failed.json, clients.json,
// and optionally ops/<opID>.json from the host via SFTP. Missing files are left nil.
func ReadHostStateFiles(ctx context.Context, c *ssh.Client, probe *HostProbe, opID string) HostStateFiles {
	var out HostStateFiles
	if probe == nil || probe.StateDir == "" {
		return out
	}
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return out
	}
	defer cleanup()

	readJSON := func(remotePath string, dst any) (found bool, notExist bool) {
		info, lerr := sc.Lstat(remotePath)
		if lerr != nil {
			return false, errors.Is(lerr, os.ErrNotExist)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, false
		}
		f, err := sc.Open(remotePath)
		if err != nil {
			return false, errors.Is(err, os.ErrNotExist)
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 512<<10))
		if err != nil {
			return false, false
		}
		return json.Unmarshal(b, dst) == nil, false
	}

	var inst HostInstallRecord
	if ok, _ := readJSON(probe.InstallJSON, &inst); ok {
		out.Install = &inst
	}
	var meta HostMetaRecord
	if ok, _ := readJSON(probe.MetaJSON, &meta); ok {
		out.Meta = &meta
	}
	var failed HostFailedRecord
	if ok, _ := readJSON(probe.FailedJSON, &failed); ok {
		out.Failed = &failed
	}
	if probe.ClientsJSON != "" {
		var cf HostClientsFile
		if ok, notExist := readJSON(probe.ClientsJSON, &cf); ok {
			out.Clients = cf.Clients
			out.ClientsLoaded = true
		} else if notExist {
			out.ClientsLoaded = true
		}
	}
	if opID != "" && opIDRE.MatchString(opID) {
		var opRec HostOpRecord
		if ok, _ := readJSON(probe.OpsDir+"/"+opID+".json", &opRec); ok {
			out.Op = &opRec
		}
	}
	return out
}

// RecordFailedOnHost appends release to ~/.local/state/corral/corrald/<mid>/failed.json
// via SFTP so neither this gateway nor another retries a broken release.
func RecordFailedOnHost(ctx context.Context, c *ssh.Client, home string, expectedUID int64, failedPath string, release int64, reason string) error {
	if failedPath == "" || release <= 0 {
		return nil
	}
	rec := &HostFailedRecord{}
	if b, err := sftpReadFile(ctx, c, failedPath, 256<<10); err == nil {
		_ = json.Unmarshal(b, rec)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	found := false
	for i := range rec.Releases {
		if rec.Releases[i].Release == release {
			rec.Releases[i].Reason = reason
			rec.Releases[i].At = now
			found = true
			break
		}
	}
	if !found {
		rec.Releases = append(rec.Releases, HostFailedEntry{
			Release: release,
			Reason:  reason,
			At:      now,
		})
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return sftpWriteFile(ctx, c, home, expectedUID, failedPath, b)
}

// ValidateProbe checks whether the probed host satisfies corrald requirements.
func ValidateProbe(probe *HostProbe, dist *Dist) (*ArchBinary, *OpFailure) {
	if probe == nil {
		return nil, &OpFailure{Code: "disconnected", Detail: "host probe unavailable"}
	}
	if !strings.EqualFold(probe.OS, "Linux") {
		return nil, &OpFailure{
			Code: "unsupported_os",
			OS:   FriendlyOSName(probe.OS),
			Arch: probe.Machine,
		}
	}
	if probe.Arch == "" {
		return nil, &OpFailure{
			Code: "unsupported_arch",
			OS:   probe.OS,
			Arch: probe.Machine,
		}
	}
	bin, ok := dist.Binary(probe.Arch)
	if !ok {
		return nil, &OpFailure{
			Code: "unsupported_arch",
			OS:   probe.OS,
			Arch: probe.Machine,
		}
	}
	if probe.HasTombstone {
		return nil, &OpFailure{
			Code:      proto.ErrRemoving,
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
		}
	}
	if !probe.HomeWritable || probe.Home == "" || probe.StateDir == "" {
		return nil, &OpFailure{
			Code:      "home_readonly",
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
		}
	}
	if probe.FreeKB >= 0 && probe.FreeKB < MinFreeKB {
		return nil, &OpFailure{
			Code:      "no_space",
			Detail:    fmt.Sprintf("free %d KB, need %d KB", probe.FreeKB, MinFreeKB),
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
		}
	}
	return bin, nil
}

// BusyRetryPolicy configures automatic retries when a host-side corrald
// lifecycle command returns `busy` (lock contention with another gateway or
// concurrent lifecycle operation).
type BusyRetryPolicy struct {
	Interval   time.Duration
	MaxRetries int
}

// DefaultBusyRetryPolicy retries up to 12 times at 5-second intervals.
var DefaultBusyRetryPolicy = BusyRetryPolicy{
	Interval:   5 * time.Second,
	MaxRetries: 12,
}

func (p BusyRetryPolicy) effective() (time.Duration, int) {
	iv := p.Interval
	if iv <= 0 {
		iv = DefaultBusyRetryPolicy.Interval
	}
	maxR := p.MaxRetries
	if maxR < 0 {
		maxR = 0
	}
	return iv, maxR
}

func waitBusyRetry(ctx context.Context, d time.Duration, isCancelled func() bool) bool {
	if isCancelled != nil && isCancelled() {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return isCancelled == nil || !isCancelled()
	case <-ctx.Done():
		return false
	}
}

// StageAndInstall uploads, verifies, selftests, and installs corrald on the host.
// On upgrade=true it passes --upgrade so the symlink is not switched until handoff.
func StageAndInstall(
	ctx context.Context,
	c *ssh.Client,
	dist *Dist,
	probe *HostProbe,
	bin *ArchBinary,
	opID, gatewayID, byUser string,
	upgrade bool,
	onStep func(step string),
	isCancelled func() bool,
	verifyHook func(actual string) string,
) (installedBin string, fail *OpFailure) {
	return StageAndInstallWithRetry(ctx, c, dist, probe, bin, opID, gatewayID, byUser, upgrade, DefaultBusyRetryPolicy, onStep, isCancelled, verifyHook)
}

// StageAndInstallWithRetry is StageAndInstall with a configurable busy retry policy.
func StageAndInstallWithRetry(
	ctx context.Context,
	c *ssh.Client,
	dist *Dist,
	probe *HostProbe,
	bin *ArchBinary,
	opID, gatewayID, byUser string,
	upgrade bool,
	busyRetry BusyRetryPolicy,
	onStep func(step string),
	isCancelled func() bool,
	verifyHook func(actual string) string,
) (installedBin string, fail *OpFailure) {
	stageDir := probe.StateDir + "/stage-" + opID
	stageBin := stageDir + "/corrald"

	cleanupStage := func() []LeftItem {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, err := runSSH(cctx, c, RmStageCommand(stageDir), 4096, sshx.ExecOptions{})
		if err != nil || ParseKeyValues(r.Out)["result"] != "ok" {
			reason := "cleanup failed"
			if err != nil {
				reason = err.Error()
			}
			return []LeftItem{{Path: stageDir, Reason: reason}}
		}
		return nil
	}

	checkCancel := func() *OpFailure {
		if isCancelled != nil && isCancelled() {
			_ = cleanupStage()
			return &OpFailure{Code: "cancelled"}
		}
		return nil
	}

	// Step: upload
	if onStep != nil {
		onStep("upload")
	}
	if cf := checkCancel(); cf != nil {
		return "", cf
	}

	mkRes, mkErr := runSSH(ctx, c, MkStageCommand(probe.Home, probe.StateDir, stageDir), 4096, sshx.ExecOptions{})
	mkFields := ParseKeyValues(mkRes.Out)
	if mkFields["error"] == proto.ErrRemoving {
		return "", &OpFailure{Code: proto.ErrRemoving, MachineID: probe.MachineID, OS: probe.OS, Arch: probe.Arch}
	}
	if mkErr != nil || mkFields["result"] != "ok" {
		if !mkRes.Ended && ctx.Err() == nil {
			return "", &OpFailure{Code: "disconnected", Detail: errStr(mkErr), MachineID: probe.MachineID, OS: probe.OS, Arch: probe.Arch}
		}
		if isCancelled != nil && isCancelled() {
			return "", &OpFailure{Code: "cancelled"}
		}
		return "", &OpFailure{Code: classifyWriteErr(mkErr, mkFields["error"]), Detail: firstNonEmpty(mkFields["detail"], mkFields["error"], errStr(mkErr)), MachineID: probe.MachineID, OS: probe.OS, Arch: probe.Arch}
	}

	if err := sftpUploadBinarySecure(ctx, c, bin.Path, probe.Home, probe.UIDInt64(), probe.LibDir, stageDir, stageBin); err != nil {
		if isCancelled != nil && isCancelled() {
			_ = cleanupStage()
			return "", &OpFailure{Code: "cancelled"}
		}
		left := cleanupStage()
		return "", &OpFailure{
			Code:      classifySFTPErr(err),
			Detail:    err.Error(),
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
			Left:      left,
		}
	}

	if cf := checkCancel(); cf != nil {
		return "", cf
	}

	// Step: verify (SFTP readback SHA-256 + selftest)
	if onStep != nil {
		onStep("verify")
	}
	gotSHA, rerr := sftpReadbackSHA256(ctx, c, stageBin)
	if rerr != nil {
		if isCancelled != nil && isCancelled() {
			_ = cleanupStage()
			return "", &OpFailure{Code: "cancelled"}
		}
		left := cleanupStage()
		return "", &OpFailure{
			Code:      classifySFTPErr(rerr),
			Detail:    rerr.Error(),
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
			Left:      left,
		}
	}
	if verifyHook != nil {
		gotSHA = verifyHook(gotSHA)
	}
	if !strings.EqualFold(gotSHA, bin.SHA256) {
		left := cleanupStage()
		return "", &OpFailure{
			Code:      "checksum_mismatch",
			Detail:    fmt.Sprintf("got %s, want %s", gotSHA, bin.SHA256),
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
			Left:      left,
		}
	}

	if cf := checkCancel(); cf != nil {
		return "", cf
	}

	stRes, stErr := runSSH(ctx, c, SelftestCommand(stageBin), 16<<10, sshx.ExecOptions{})
	stFields := ParseKeyValues(stRes.Out)
	if stErr != nil || stFields["result"] != "ok" {
		if isCancelled != nil && isCancelled() {
			_ = cleanupStage()
			return "", &OpFailure{Code: "cancelled"}
		}
		left := cleanupStage()
		if !stRes.Ended {
			return "", &OpFailure{Code: "disconnected", Detail: errStr(stErr), MachineID: probe.MachineID, OS: probe.OS, Arch: probe.Arch, Left: left}
		}
		code := classifySelftestErr(stFields["error"], stFields["detail"], stErr)
		return "", &OpFailure{
			Code:      code,
			Detail:    firstNonEmpty(stFields["detail"], stFields["error"], errStr(stErr)),
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
			Left:      left,
		}
	}
	// Update probe socket path from selftest authoritative output if reported.
	if sock := stFields["socket"]; sock != "" {
		probe.SocketPath = sock
	}

	if cf := checkCancel(); cf != nil {
		return "", cf
	}

	// Step: install (auto-retry on `busy` while preserving stage directory)
	if onStep != nil {
		onStep("install")
	}
	iv, maxRetries := busyRetry.effective()
	var inRes sshx.ExecResult
	var inErr error
	var inFields map[string]string
	for attempt := 0; ; attempt++ {
		inRes, inErr = runSSH(ctx, c, InstallCommand(stageBin, stageDir, opID, gatewayID, byUser, upgrade), 16<<10, sshx.ExecOptions{})
		inFields = ParseKeyValues(inRes.Out)
		if inFields["error"] == proto.ErrBusy && attempt < maxRetries {
			if waitBusyRetry(ctx, iv, isCancelled) {
				continue
			}
			_ = cleanupStage()
			return "", &OpFailure{Code: "cancelled"}
		}
		break
	}
	if inFields["error"] == proto.ErrRemoving {
		_ = cleanupStage()
		return "", &OpFailure{Code: proto.ErrRemoving, MachineID: probe.MachineID, OS: probe.OS, Arch: probe.Arch}
	}
	if inErr != nil || inFields["result"] != "ok" {
		if isCancelled != nil && isCancelled() {
			_ = cleanupStage()
			return "", &OpFailure{Code: "cancelled"}
		}
		left := cleanupStage()
		if !inRes.Ended {
			return "", &OpFailure{Code: "disconnected", Detail: errStr(inErr), MachineID: probe.MachineID, OS: probe.OS, Arch: probe.Arch, Left: left}
		}
		code := classifyInstallErr(inFields["error"], inFields["detail"])
		return "", &OpFailure{
			Code:      code,
			Detail:    firstNonEmpty(inFields["detail"], inFields["error"], errStr(inErr)),
			MachineID: probe.MachineID,
			OS:        probe.OS,
			Arch:      probe.Arch,
			Left:      left,
		}
	}

	installedBin = inFields["binary"]
	if installedBin == "" {
		installedBin = path.Join(probe.LibDir, fmt.Sprintf("corrald-%s-%d", dist.Version, dist.Release), "corrald")
	}
	return installedBin, nil
}

// RunServe starts corrald detached via `corrald serve --gateway <gw>` and
// returns its PID and Unix socket path.
func RunServe(ctx context.Context, c *ssh.Client, binPath, gatewayID, machineID, osName, arch string) (pid int, socketPath string, fail *OpFailure) {
	return RunServeWithRetry(ctx, c, binPath, gatewayID, machineID, osName, arch, DefaultBusyRetryPolicy)
}

// RunServeWithRetry starts corrald detached and automatically retries on `busy`.
func RunServeWithRetry(ctx context.Context, c *ssh.Client, binPath, gatewayID, machineID, osName, arch string, busyRetry BusyRetryPolicy) (pid int, socketPath string, fail *OpFailure) {
	iv, maxRetries := busyRetry.effective()
	var res sshx.ExecResult
	var err error
	var f map[string]string
	for attempt := 0; ; attempt++ {
		res, err = runSSH(ctx, c, ServeCommand(binPath, gatewayID), 16<<10, sshx.ExecOptions{})
		f = ParseKeyValues(res.Out)
		if f["error"] == proto.ErrBusy && attempt < maxRetries {
			if waitBusyRetry(ctx, iv, nil) {
				continue
			}
			return 0, "", &OpFailure{Code: proto.ErrBusy, Detail: firstNonEmpty(f["detail"], "busy"), MachineID: machineID, OS: osName, Arch: arch, KeepInstalled: true}
		}
		break
	}
	if f["error"] == proto.ErrRemoving {
		return 0, "", &OpFailure{Code: proto.ErrRemoving, MachineID: machineID, OS: osName, Arch: arch}
	}
	if f["error"] == proto.ErrBusy {
		return 0, "", &OpFailure{Code: proto.ErrBusy, Detail: firstNonEmpty(f["detail"], "busy"), MachineID: machineID, OS: osName, Arch: arch, KeepInstalled: true}
	}
	if err != nil && !res.Ended {
		return 0, "", &OpFailure{Code: "disconnected", Detail: err.Error(), MachineID: machineID, OS: osName, Arch: arch, KeepInstalled: true}
	}
	if f["result"] == "ok" || f["result"] == "running" {
		pid, _ = strconv.Atoi(f["pid"])
		return pid, f["socket"], nil
	}
	return 0, "", &OpFailure{
		Code:          "start_failed",
		Detail:        firstNonEmpty(f["detail"], f["error"], errStr(err)),
		MachineID:     machineID,
		OS:            osName,
		Arch:          arch,
		KeepInstalled: true,
	}
}

// RunUninstallSoleGatewayWithRetry runs host-side atomic sole-gateway uninstall
// (--only-if-sole-gateway) without writing a premature tombstone before the
// host confirms exclusivity under .corrald-shared.lock -> op.lock -> corrald.lock.
//
// If the host binary was already deleted during an earlier partial uninstall
// leaving a removing tombstone behind (error=binary_missing_with_tombstone) and
// dist is available, it uploads the matching Go corrald binary to a private
// temporary directory and runs the full locked Go uninstall path instead of an
// unlocked shell fallback.
//
// Possible Result values:
//   - "removed": confirmed sole gateway and files removed
//   - "other_gateways": confirmed another gateway is connected/registered (files preserved)
//   - "unsupported": host binary/daemon does not support conditional shutdown/uninstall (files preserved)
//   - "partial" / "stop_failed" / "pending": transient or incomplete failure (caller keeps remove_pending)
func RunUninstallSoleGatewayWithRetry(ctx context.Context, c *ssh.Client, dist *Dist, probe *HostProbe, opID, soleGateway string, busyRetry BusyRetryPolicy, onStep func(step string)) UninstallOutcome {
	if soleGateway == "" {
		return UninstallOutcome{Result: "pending", Detail: "gateway_id is empty"}
	}
	if onStep != nil {
		onStep("remove")
	}
	iv, maxRetries := busyRetry.effective()
	var res sshx.ExecResult
	var err error
	var f map[string]string
	for attempt := 0; ; attempt++ {
		res, err = runSSH(ctx, c, UninstallCommandSoleGateway(probe.LibDir, probe.StateDir, probe.SocketPath, opID, soleGateway), 64<<10, sshx.ExecOptions{})
		if err != nil && !res.Ended {
			return UninstallOutcome{Result: "pending", Detail: err.Error()}
		}
		f = ParseKeyValues(res.Out)
		if f["error"] == proto.ErrBusy && attempt < maxRetries {
			if waitBusyRetry(ctx, iv, nil) {
				continue
			}
			return UninstallOutcome{Result: "pending", Detail: firstNonEmpty(f["detail"], "busy")}
		}
		break
	}

	if f["error"] == "binary_missing_with_tombstone" {
		if dist == nil {
			return UninstallOutcome{Result: "pending", Detail: firstNonEmpty(f["detail"], f["error"])}
		}
		return runTempBinarySoleGatewayUninstall(ctx, c, dist, probe, opID, soleGateway, busyRetry, onStep)
	}

	if onStep != nil {
		onStep("verify")
	}
	return parseSoleGatewayUninstallOutcome(res, err, f)
}

func runTempBinarySoleGatewayUninstall(ctx context.Context, c *ssh.Client, dist *Dist, probe *HostProbe, opID, soleGateway string, busyRetry BusyRetryPolicy, onStep func(step string)) UninstallOutcome {
	bin, vfail := ValidateProbe(probe, dist)
	if vfail != nil {
		return UninstallOutcome{Result: "pending", Detail: firstNonEmpty(vfail.Detail, vfail.Code)}
	}
	if opID == "" {
		opID = newHexID()
	}
	home := strings.TrimRight(probe.Home, "/")
	if home == "" {
		return UninstallOutcome{Result: "pending", Detail: "probe home is empty"}
	}
	tempDir := home + "/.local/state/corral/.rm-" + opID
	tempBin := tempDir + "/corrald"

	mkRes, mkErr := runSSH(ctx, c, MkRecoveryDirCommand(probe.Home, tempDir), 4096, sshx.ExecOptions{})
	mkFields := ParseKeyValues(mkRes.Out)
	if mkErr != nil || mkFields["result"] != "ok" {
		return UninstallOutcome{Result: "pending", Detail: firstNonEmpty(mkFields["detail"], mkFields["error"], errStr(mkErr))}
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = runSSH(cctx, c, RmRecoveryDirCommand(tempDir), 4096, sshx.ExecOptions{})
	}()

	if err := sftpUploadBinarySecure(ctx, c, bin.Path, probe.Home, probe.UIDInt64(), probe.LibDir, tempDir, tempBin); err != nil {
		return UninstallOutcome{Result: "pending", Detail: err.Error()}
	}
	gotSHA, rerr := sftpReadbackSHA256(ctx, c, tempBin)
	if rerr != nil || !strings.EqualFold(gotSHA, bin.SHA256) {
		detail := "recovery binary checksum mismatch"
		if rerr != nil {
			detail = rerr.Error()
		}
		return UninstallOutcome{Result: "pending", Detail: detail}
	}

	iv, maxRetries := busyRetry.effective()
	var res sshx.ExecResult
	var err error
	var f map[string]string
	for attempt := 0; ; attempt++ {
		res, err = runSSH(ctx, c, RunBinaryUninstallSoleGatewayCommand(tempBin, opID, soleGateway), 64<<10, sshx.ExecOptions{})
		if err != nil && !res.Ended {
			return UninstallOutcome{Result: "pending", Detail: err.Error()}
		}
		f = ParseKeyValues(res.Out)
		if f["error"] == proto.ErrBusy && attempt < maxRetries {
			if waitBusyRetry(ctx, iv, nil) {
				continue
			}
			return UninstallOutcome{Result: "pending", Detail: firstNonEmpty(f["detail"], "busy")}
		}
		break
	}
	if onStep != nil {
		onStep("verify")
	}
	return parseSoleGatewayUninstallOutcome(res, err, f)
}

func parseSoleGatewayUninstallOutcome(res sshx.ExecResult, err error, f map[string]string) UninstallOutcome {
	switch {
	case f["result"] == "removed":
		return UninstallOutcome{Result: "removed", LastMachine: f["last_machine"] == "true"}
	case f["result"] == "other_gateways":
		return UninstallOutcome{Result: "other_gateways", Detail: f["detail"]}
	case f["error"] == "unsupported_conditional_shutdown" || f["error"] == "invalid_args":
		return UninstallOutcome{Result: "unsupported", Detail: firstNonEmpty(f["detail"], f["error"])}
	case f["error"] == "stop_failed":
		pid, _ := strconv.Atoi(f["pid"])
		return UninstallOutcome{Result: "stop_failed", PID: pid, Detail: f["detail"]}
	case f["result"] == "partial":
		return UninstallOutcome{Result: "partial", Left: ParseLeftLines(res.Out), LastMachine: f["last_machine"] == "true"}
	default:
		detail := firstNonEmpty(f["detail"], f["error"], errStr(err))
		return UninstallOutcome{Result: "pending", Detail: detail}
	}
}

// RunUninstall writes the removing tombstone and executes corrald uninstall.
func RunUninstall(ctx context.Context, c *ssh.Client, probe *HostProbe, opID string, onStep func(step string)) UninstallOutcome {
	return RunUninstallWithRetry(ctx, c, probe, opID, DefaultBusyRetryPolicy, onStep)
}

// RunUninstallWithRetry writes the removing tombstone and executes corrald
// uninstall, automatically retrying on `busy`.
func RunUninstallWithRetry(ctx context.Context, c *ssh.Client, probe *HostProbe, opID string, busyRetry BusyRetryPolicy, onStep func(step string)) UninstallOutcome {
	if onStep != nil {
		onStep("mark")
	}
	if probe.StateDir != "" {
		mres, merr := runSSH(ctx, c, WriteTombstoneCommand(probe.StateDir), 4096, sshx.ExecOptions{})
		if merr != nil && !mres.Ended {
			return UninstallOutcome{Result: "pending", Detail: merr.Error()}
		}
	}
	if onStep != nil {
		onStep("stop")
	}
	// uninstall performs stop + remove + post-check verification atomically on the host.
	if onStep != nil {
		onStep("remove")
	}
	iv, maxRetries := busyRetry.effective()
	var res sshx.ExecResult
	var err error
	var f map[string]string
	for attempt := 0; ; attempt++ {
		res, err = runSSH(ctx, c, UninstallCommand(probe.LibDir, probe.StateDir, probe.SocketPath, opID), 64<<10, sshx.ExecOptions{})
		if err != nil && !res.Ended {
			return UninstallOutcome{Result: "pending", Detail: err.Error()}
		}
		f = ParseKeyValues(res.Out)
		if f["error"] == proto.ErrBusy && attempt < maxRetries {
			if waitBusyRetry(ctx, iv, nil) {
				continue
			}
			return UninstallOutcome{Result: "pending", Detail: firstNonEmpty(f["detail"], "busy")}
		}
		break
	}
	if onStep != nil {
		onStep("verify")
	}
	switch {
	case f["result"] == "removed":
		return UninstallOutcome{Result: "removed", LastMachine: f["last_machine"] == "true"}
	case f["error"] == "stop_failed":
		pid, _ := strconv.Atoi(f["pid"])
		return UninstallOutcome{Result: "stop_failed", PID: pid, Detail: f["detail"]}
	case f["error"] == proto.ErrBusy:
		return UninstallOutcome{Result: "pending", Detail: firstNonEmpty(f["detail"], "busy")}
	case f["result"] == "partial":
		return UninstallOutcome{Result: "partial", Left: ParseLeftLines(res.Out), LastMachine: f["last_machine"] == "true"}
	default:
		if err != nil {
			return UninstallOutcome{Result: "pending", Detail: err.Error()}
		}
		return UninstallOutcome{Result: "removed"}
	}
}

func classifySFTPErr(err error) string {
	if err == nil {
		return "other"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no space"), strings.Contains(msg, "enospc"), strings.Contains(msg, "quota"):
		return "no_space"
	case strings.Contains(msg, "unsafe"), strings.Contains(msg, "symlink"), strings.Contains(msg, "permission denied"), strings.Contains(msg, "read-only"), strings.Contains(msg, "erofs"):
		return "home_readonly"
	case strings.Contains(msg, "eof"), strings.Contains(msg, "closed"), strings.Contains(msg, "connection"), strings.Contains(msg, "broken pipe"):
		return "disconnected"
	default:
		return "disconnected"
	}
}

func classifyWriteErr(err error, code string) string {
	msg := strings.ToLower(errStr(err) + " " + code)
	if strings.Contains(msg, "no space") || strings.Contains(msg, "enospc") || strings.Contains(msg, "quota") {
		return "no_space"
	}
	return "home_readonly"
}

func classifySelftestErr(code, detail string, err error) string {
	msg := strings.ToLower(code + " " + detail + " " + errStr(err))
	switch {
	case code == "noexec", code == "chmod_failed",
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "read-only"),
		strings.Contains(msg, "noexec"),
		strings.Contains(msg, "exit status 126"),
		strings.Contains(msg, "status 126"):
		return "home_readonly"
	case strings.Contains(msg, "no space"), strings.Contains(msg, "enospc"), strings.Contains(msg, "quota"):
		return "no_space"
	case code == "unsupported_os":
		return "unsupported_os"
	case code == "unsupported_arch":
		return "unsupported_arch"
	default:
		return "start_failed"
	}
}

func classifyInstallErr(code, detail string) string {
	msg := strings.ToLower(code + " " + detail)
	switch {
	case code == proto.ErrRemoving:
		return proto.ErrRemoving
	case code == proto.ErrBusy:
		return proto.ErrBusy
	case strings.Contains(msg, "no space"), strings.Contains(msg, "enospc"), strings.Contains(msg, "quota"):
		return "no_space"
	case strings.Contains(msg, "permission denied"), strings.Contains(msg, "read-only"), code == "mkdir_failed", code == "state_dir_failed":
		return "home_readonly"
	default:
		return "start_failed"
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
