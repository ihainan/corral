package web

// VS Code in the browser: code-server on the host, installed and started on
// demand over a forwarding connection of its own, and served like a private
// forward on "code-<id>.<forward domain>" (same tickets, cookie, per-request
// checks and revocation as forwarded ports).
//
// Starting is a background job the browser polls; there is at most one per
// owner and host. Its state lives in memory: after a gateway restart the
// next start finds code-server still running and reuses it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/codeserver"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

// Code-server job states, as the API reports them.
const (
	codeStopped     = "stopped"
	codeChecking    = "checking"
	codeDownloading = "downloading"
	codeUploading   = "uploading"
	codeExtracting  = "extracting"
	codeStarting    = "starting"
	codeReady       = "ready"
	codeFailed      = "failed"
)

// Limits and timeouts; variables so tests can lower them.
var (
	maxCodeJobsPerUser  = 3
	codeStepTimeout     = 2 * time.Minute  // checking, extracting, starting, stopping
	codeInstallTimeout  = 15 * time.Minute // downloading or uploading, and unpacking, the release
	codeHealthAttempts  = 20
	codeHealthInterval  = 500 * time.Millisecond
	codeHealthPerTryMax = 5 * time.Second
)

type codeStatus struct {
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`   // failure code, for the interface
	Message string `json:"message,omitempty"` // failure details
	URL     string `json:"url,omitempty"`     // when ready: opens VS Code (ticket hand-off)
}

type codeJob struct {
	status  codeStatus
	socket  string // where code-server listens, once ready
	running bool   // the start job is still working
	cancel  context.CancelFunc
}

type codeServers struct {
	mu     sync.Mutex
	jobs   map[[2]int64]*codeJob // by owner and host
	closed bool
	wg     sync.WaitGroup
}

func (c *codeServers) status(owner, host int64) codeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.jobs[[2]int64{owner, host}]; j != nil {
		return j.status
	}
	return codeStatus{State: codeStopped}
}

// socket is the code-server socket of a ready instance, or "".
func (c *codeServers) socket(owner, host int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.jobs[[2]int64{owner, host}]; j != nil && j.status.State == codeReady {
		return j.socket
	}
	return ""
}

func (c *codeServers) update(key [2]int64, j *codeJob, f func(*codeJob)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobs[key] == j { // not forgotten meanwhile
		f(j)
	}
}

// forget drops matching jobs, cancelling the ones still working.
func (c *codeServers) forget(match func(key [2]int64) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, j := range c.jobs {
		if match(k) {
			if j.cancel != nil {
				j.cancel()
			}
			delete(c.jobs, k)
		}
	}
}

func (c *codeServers) shutdown() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.forget(func([2]int64) bool { return true })
	c.wg.Wait()
}

// forgetHostCode ends what Corral knows about code-server on a host (deleted
// or moved to another address).
func (s *Server) forgetHostCode(owner, host int64) {
	s.code.forget(func(k [2]int64) bool { return k == [2]int64{owner, host} })
}

func (s *Server) forgetUserCode(owner int64) {
	s.code.forget(func(k [2]int64) bool { return k[0] == owner })
}

// API ----------------------------------------------------------------------

// codeHost resolves the host for the code routes: owned, code enabled.
func (s *Server) codeHost(w http.ResponseWriter, r *http.Request) (*store.Host, bool) {
	if !s.cfg.CodeServer {
		writeError(w, http.StatusNotFound, "code_disabled", "VS Code is not set up on this server")
		return nil, false
	}
	return s.ownedHost(w, r)
}

// GET /api/hosts/{id}/code
func (s *Server) apiCodeStatus(w http.ResponseWriter, r *http.Request) {
	h, ok := s.codeHost(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.code.status(h.OwnerID, h.ID))
}

// POST /api/hosts/{id}/code starts code-server (installing it first if
// needed) in the background: 202 and the state to poll.
func (s *Server) apiCodeStart(w http.ResponseWriter, r *http.Request) {
	h, ok := s.codeHost(w, r)
	if !ok {
		return
	}
	if h.Auth == store.AuthAsk || h.CredentialID == nil {
		writeError(w, http.StatusConflict, "needs_credential", "VS Code needs a saved password or key for this host")
		return
	}
	f, err := s.store.EnsureCodeForward(r.Context(), h.OwnerID, h.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	key := [2]int64{h.OwnerID, h.ID}
	s.code.mu.Lock()
	if s.code.closed {
		s.code.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "busy", "the server is shutting down")
		return
	}
	if j := s.code.jobs[key]; j != nil && j.running {
		st := j.status
		s.code.mu.Unlock()
		writeJSON(w, http.StatusAccepted, st)
		return
	}
	n := 0
	for k, j := range s.code.jobs {
		if k[0] == h.OwnerID && j.running {
			n++
		}
	}
	if n >= maxCodeJobsPerUser {
		s.code.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "busy", "VS Code is already starting on other hosts; wait for them first")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*codeInstallTimeout+3*codeStepTimeout)
	j := &codeJob{status: codeStatus{State: codeChecking}, running: true, cancel: cancel}
	s.code.jobs[key] = j
	s.code.wg.Add(1)
	s.code.mu.Unlock()
	go func() {
		defer s.code.wg.Done()
		defer cancel()
		s.runCode(ctx, key, j, h, f)
	}()
	s.audit(r, "code.start", hostTarget(h), map[string]any{"version": s.cfg.CodeServerVersion})
	writeJSON(w, http.StatusAccepted, j.status)
}

// POST /api/hosts/{id}/code/stop stops code-server on the host.
func (s *Server) apiCodeStop(w http.ResponseWriter, r *http.Request) {
	h, ok := s.codeHost(w, r)
	if !ok {
		return
	}
	s.forgetHostCode(h.OwnerID, h.ID)
	f, err := s.store.EnsureCodeForward(r.Context(), h.OwnerID, h.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.revokeForwardGrants(f.ID, 0) // open VS Code tabs end now
	if h.Auth != store.AuthAsk && h.CredentialID != nil {
		ctx, cancel := context.WithTimeout(r.Context(), codeStepTimeout)
		defer cancel()
		if err := s.stopCode(ctx, f); err != nil {
			writeError(w, http.StatusBadGateway, "code_unreachable", "could not reach the host to stop VS Code: "+err.Error())
			return
		}
	}
	s.audit(r, "code.stop", hostTarget(h), nil)
	writeJSON(w, http.StatusOK, codeStatus{State: codeStopped})
}

// The job --------------------------------------------------------------------

type codeFailure struct{ code, msg string }

func (e *codeFailure) Error() string { return e.msg }

func codeFail(code, format string, args ...any) error {
	return &codeFailure{code: code, msg: fmt.Sprintf(format, args...)}
}

func (s *Server) runCode(ctx context.Context, key [2]int64, j *codeJob, h *store.Host, f *store.Forward) {
	step := func(state string) {
		s.code.update(key, j, func(j *codeJob) { j.status = codeStatus{State: state} })
	}
	sock, err := s.startCode(ctx, h, f, step)
	s.code.update(key, j, func(j *codeJob) {
		j.running = false
		if err == nil {
			j.socket = sock
			j.status = codeStatus{State: codeReady, URL: s.cfg.Origin() + "/fwd/" + f.ID}
			return
		}
		var cf *codeFailure
		if !errors.As(err, &cf) {
			cf = &codeFailure{code: "failed", msg: err.Error()}
		}
		if ctx.Err() != nil {
			cf = &codeFailure{code: "timeout", msg: "starting VS Code took too long"}
		}
		j.status = codeStatus{State: codeFailed, Error: cf.code, Message: cf.msg}
	})
}

// codeRun runs one of the codeserver commands and returns its answer fields.
func (s *Server) codeRun(ctx context.Context, fc *fwdConn, cmd string, limit time.Duration) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := sshx.Output(ctx, fc.client, cmd, sshx.MaxOutput)
	fields := codeserver.Fields(out)
	if err != nil && fields["error"] == "" {
		if errors.Is(err, sshx.ErrBusy) {
			return nil, codeFail("busy", "earlier commands on this host are still running; try again in a moment")
		}
		if ctx.Err() != nil {
			return nil, codeFail("timeout", "the host did not answer in time")
		}
		return nil, codeFail("failed", "a command on the host failed: %v", err)
	}
	return fields, nil
}

// codeConn opens (or reuses) the forwarding connection for VS Code.
func (s *Server) codeConn(ctx context.Context, f *store.Forward) (*fwdConn, error) {
	fc, err := s.forwardConn(ctx, f)
	if errors.Is(err, errFwdBusy) {
		return nil, codeFail("busy", "too many forwarding connections are open; try again in a moment")
	}
	if err != nil {
		return nil, codeFail("connect", "could not connect to the host: %v", err)
	}
	return fc, nil
}

// probeCode checks the host and works out the socket path.
func (s *Server) probeCode(ctx context.Context, fc *fwdConn) (*codeserver.Probe, string, error) {
	fields, err := s.codeRun(ctx, fc, codeserver.ProbeCommand(s.cfg.CodeServerVersion), codeStepTimeout)
	if err != nil {
		return nil, "", err
	}
	p, err := codeserver.ParseProbe(fields)
	if errors.Is(err, codeserver.ErrArch) {
		return nil, "", codeFail("arch", "%v", err)
	}
	if err != nil {
		return nil, "", codeFail("failed", "%v", err)
	}
	sock, err := codeserver.SocketPath(p.Home, p.UID)
	if err != nil {
		return nil, "", codeFail("socket", "%v", err)
	}
	return p, sock, nil
}

func (s *Server) startCode(ctx context.Context, h *store.Host, f *store.Forward, step func(string)) (string, error) {
	fc, err := s.codeConn(ctx, f)
	if err != nil {
		return "", err
	}
	defer s.fwd.release(fc)
	p, sock, err := s.probeCode(ctx, fc)
	if err != nil {
		return "", err
	}
	ver := s.cfg.CodeServerVersion
	if !p.Installed {
		if !p.Tar {
			return "", codeFail("no_tar", "the host has no tar, which unpacking code-server needs")
		}
		if p.FreeKB >= 0 && p.FreeKB < codeserver.MinFreeKB {
			return "", codeFail("no_space", "only %d MB are free in the home directory; installing VS Code needs about %d MB",
				p.FreeKB>>10, codeserver.MinFreeKB>>10)
		}
		var archive string
		if s.cfg.CodeServerDir != "" {
			step(codeUploading)
			if err := s.uploadCode(ctx, fc, p.Arch); err != nil {
				return "", err
			}
			archive = codeserver.UploadPath(ver, p.Arch)
		} else {
			if !p.Curl && !p.Wget {
				return "", codeFail("no_downloader", "the host has neither curl nor wget to download code-server; "+
					"put the release in CORRAL_CODE_SERVER_DIR on the gateway to upload it instead")
			}
			step(codeDownloading)
			fields, err := s.codeRun(ctx, fc, codeserver.DownloadCommand(ver, p.Arch, s.cfg.CodeServerDownload(p.Arch)), codeInstallTimeout)
			if err != nil {
				return "", err
			}
			if fields["result"] != "ok" {
				return "", codeFail("download", "the host could not download code-server (%s); if it has no internet access, "+
					"put %s in CORRAL_CODE_SERVER_DIR on the gateway to upload it instead",
					strings.TrimSpace(fields["detail"]), s.cfg.CodeServerFile(p.Arch))
			}
			archive = codeserver.DownloadPath(ver, p.Arch)
		}
		step(codeExtracting)
		fields, err := s.codeRun(ctx, fc, codeserver.ExtractCommand(ver, p.Arch, archive), codeInstallTimeout) // a few hundred MB on slow disks
		if err != nil {
			return "", err
		}
		if fields["result"] != "ok" {
			return "", codeFail("extract", "unpacking code-server failed (%s %s)", fields["error"], strings.TrimSpace(fields["detail"]))
		}
	}
	step(codeStarting)
	fields, err := s.codeRun(ctx, fc, codeserver.StartCommand(ver, sock), codeStepTimeout)
	if err != nil {
		return "", err
	}
	if r := fields["result"]; r != "started" && r != "running" {
		return "", codeFail("start", "code-server did not start (%s %s); its log is ~/.local/state/corral/code-server.log on the host",
			fields["error"], strings.TrimSpace(fields["detail"]))
	}
	s.fwd.setCodeSocket(fc, sock)
	if err := s.codeHealthy(ctx, fc); err != nil {
		return "", codeFail("start", "code-server does not answer on its socket (%v); its log is ~/.local/state/corral/code-server.log on the host", err)
	}
	return sock, nil
}

// codeHealthy waits for code-server to answer /healthz on its socket.
func (s *Server) codeHealthy(ctx context.Context, fc *fwdConn) error {
	client := &http.Client{Transport: fc.transport, Timeout: codeHealthPerTryMax,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last error
	for range codeHealthAttempts {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+codeUpstream+"/healthz", nil)
		req.Host = "localhost"
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("status %d", res.StatusCode)
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(codeHealthInterval):
		}
	}
	return last
}

// uploadCode copies the release from CORRAL_CODE_SERVER_DIR to the host.
func (s *Server) uploadCode(ctx context.Context, fc *fwdConn, arch string) error {
	name := s.cfg.CodeServerFile(arch)
	src, err := os.Open(filepath.Join(s.cfg.CodeServerDir, name))
	if err != nil {
		return codeFail("archive_missing", "%s is not in CORRAL_CODE_SERVER_DIR on the gateway", name)
	}
	defer src.Close()
	fields, err := s.codeRun(ctx, fc, codeserver.PrepareCommand(), codeStepTimeout)
	if err != nil {
		return err
	}
	if fields["result"] != "ok" {
		return codeFail("upload", "could not create ~/.local/lib/corral on the host")
	}
	client, closeSFTP, err := openSFTP(fc.client)
	if err != nil {
		return codeFail("upload", "could not open SFTP on the host: %v", err)
	}
	defer closeSFTP()
	stop := context.AfterFunc(ctx, closeSFTP) // cancelling ends a transfer at once
	defer stop()
	dst, err := client.OpenFile(codeserver.UploadPath(s.cfg.CodeServerVersion, arch), os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return codeFail("upload", "could not create the archive on the host: %v", err)
	}
	_ = dst.Chmod(0o600)
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return codeFail("upload", "uploading code-server failed: %v", err)
	}
	if err := dst.Close(); err != nil {
		return codeFail("upload", "uploading code-server failed: %v", err)
	}
	return nil
}

// stopCode stops code-server on the host (also one started before a restart
// of the gateway).
func (s *Server) stopCode(ctx context.Context, f *store.Forward) error {
	fc, err := s.codeConn(ctx, f)
	if err != nil {
		return err
	}
	defer s.fwd.release(fc)
	return s.stopCodeOn(ctx, fc)
}

// codeStopperFor prepares stopping VS Code on a host that is about to be
// deleted: what is needed to reach it is read now, the stop runs in the
// background afterwards and never holds up the delete. nil when there is
// nothing to stop or no saved way in.
func (s *Server) codeStopperFor(ctx context.Context, h *store.Host) func() {
	if !s.cfg.CodeServer || h.Auth == store.AuthAsk || h.CredentialID == nil || !s.store.HasCodeForward(ctx, h.OwnerID, h.ID) {
		return nil
	}
	_, sec, err := s.openCredential(ctx, h.OwnerID, *h.CredentialID)
	if err != nil {
		return nil
	}
	k, err := s.store.HostKey(ctx, h.OwnerID, h.ID)
	if err != nil {
		return nil
	}
	t := sshx.Target{Address: h.Address, Port: h.Port, User: h.Username, Password: sec.Password, PrivateKey: []byte(sec.PrivateKey),
		Passphrase: []byte(sec.Passphrase), Pinned: &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}}
	return func() {
		s.code.mu.Lock()
		if s.code.closed {
			s.code.mu.Unlock()
			return
		}
		s.code.wg.Add(1)
		s.code.mu.Unlock()
		go func() {
			defer s.code.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), codeStepTimeout)
			defer cancel()
			dialed, err := sshx.Dial(ctx, s.guard, t)
			if err != nil {
				return
			}
			defer dialed.Client.Close()
			if err := s.stopCodeOn(ctx, &fwdConn{client: dialed.Client}); err != nil {
				s.log.Info("could not stop VS Code on a deleted host", "host", h.Name, "err", err)
			}
		}()
	}
}

func (s *Server) stopCodeOn(ctx context.Context, fc *fwdConn) error {
	_, sock, err := s.probeCode(ctx, fc)
	if err != nil {
		return err
	}
	fields, err := s.codeRun(ctx, fc, codeserver.StopCommand(sock), codeStepTimeout)
	if err != nil {
		return err
	}
	if fields["result"] != "ok" {
		return errors.New("the stop command did not finish")
	}
	return nil
}
