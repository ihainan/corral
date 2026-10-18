package web

// VS Code in the browser: code-server on the host, installed and started on
// demand over a forwarding connection of its own, and served like a private
// forward on "code-<id>.<forward domain>" (same tickets, cookie, per-request
// checks and revocation as forwarded ports).
//
// Starting is a background job the browser polls; there is at most one per
// owner and host. Its state lives in memory: after a gateway restart the
// next start finds code-server still running and reuses it. Starts and stops
// of one host run strictly in the order they were asked for (a ticket queue);
// on the host, installing, starting and stopping also hold a lock.
//
// An install is a task the user started on the host: it carries on after a
// plain sign-out, but disabling the user, an administrator's password reset,
// deleting the host or a connection-relevant host edit cancel it. Every
// install has its own task id; a cancelled one is ended on the host
// (process group, staging directory) before the next operation runs.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	codeStopping    = "stopping"
)

// Limits and timeouts; variables so tests can lower them.
var (
	maxCodeJobsPerUser  = 3
	codeStepTimeout     = 2 * time.Minute  // checking, starting, stopping
	codeInstallTimeout  = 15 * time.Minute // downloading or uploading, checking and unpacking the release
	codeCancelTimeout   = time.Minute      // ending a cancelled install on the host
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
	status   codeStatus
	socket   string // where code-server listens, once ready
	instance string // the instance it started or found, once ready
	running  bool   // the start job is still working
	cancel   context.CancelFunc
	userGen  uint64 // the owner's revocation generation when the job was asked for
	version  int64  // the host's connection version then
}

// codeQueue hands out the turns of one host's starts and stops in order.
type codeQueue struct {
	next, serving uint64
	abandoned     map[uint64]bool
	changed       chan struct{} // closed (and replaced) when serving moves on
}

type codeServers struct {
	mu      sync.Mutex
	jobs    map[[2]int64]*codeJob // by owner and host
	stops   map[[2]int64]int      // stops queued or running
	queues  map[[2]int64]*codeQueue
	userGen map[int64]uint64 // bumped when a user's access is revoked (disable, admin reset)
	closed  bool
	wg      sync.WaitGroup
}

func (c *codeServers) status(owner, host int64) codeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := [2]int64{owner, host}
	if j := c.jobs[key]; j != nil {
		return j.status
	}
	if c.stops[key] > 0 {
		return codeStatus{State: codeStopping}
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

// enqueue takes the next turn for a host; called with mu held, together with
// registering what the turn is for, so the order of requests is the order
// of turns.
func (c *codeServers) enqueue(key [2]int64) uint64 {
	if c.queues == nil {
		c.queues = map[[2]int64]*codeQueue{}
	}
	q := c.queues[key]
	if q == nil {
		q = &codeQueue{abandoned: map[uint64]bool{}, changed: make(chan struct{})}
		c.queues[key] = q
	}
	t := q.next
	q.next++
	return t
}

// waitTurn waits until ticket t is served. Giving up passes the turn on.
func (c *codeServers) waitTurn(ctx context.Context, key [2]int64, t uint64) error {
	for {
		c.mu.Lock()
		q := c.queues[key]
		if q.serving == t {
			c.mu.Unlock()
			return nil
		}
		ch := q.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			c.mu.Lock()
			if q.serving == t {
				c.advance(key, q)
			} else {
				q.abandoned[t] = true
			}
			c.mu.Unlock()
			return ctx.Err()
		}
	}
}

// doneTurn ends the turn being served.
func (c *codeServers) doneTurn(key [2]int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(key, c.queues[key])
}

func (c *codeServers) advance(key [2]int64, q *codeQueue) {
	q.serving++
	for q.abandoned[q.serving] {
		delete(q.abandoned, q.serving)
		q.serving++
	}
	close(q.changed)
	q.changed = make(chan struct{})
	if q.serving == q.next {
		delete(c.queues, key)
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

func (c *codeServers) gen(owner int64) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userGen[owner]
}

func (c *codeServers) shutdown() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.forget(func([2]int64) bool { return true })
	c.wg.Wait()
}

// forgetHostCode ends what Corral knows about code-server on a host (deleted
// or its connection settings changed), cancelling a start in progress.
func (s *Server) forgetHostCode(owner, host int64) {
	s.code.forget(func(k [2]int64) bool { return k == [2]int64{owner, host} })
}

// forgetUserCode is for a user whose access is revoked (disabled, password
// reset by an administrator): their jobs end, and jobs asked for just before
// see the new generation.
func (s *Server) forgetUserCode(owner int64) {
	s.code.mu.Lock()
	if s.code.userGen == nil {
		s.code.userGen = map[int64]uint64{}
	}
	s.code.userGen[owner]++
	s.code.mu.Unlock()
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
	u := currentUser(r)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*codeInstallTimeout+3*codeStepTimeout+codeCancelTimeout)
	j := &codeJob{status: codeStatus{State: codeChecking}, running: true, cancel: cancel, userGen: s.code.userGen[h.OwnerID],
		version: h.ConnVersion}
	s.code.jobs[key] = j
	ticket := s.code.enqueue(key) // with the registration: after any stop asked for earlier
	first := j.status             // the job updates it from now on
	s.code.wg.Add(1)
	s.code.mu.Unlock()
	go func() {
		defer s.code.wg.Done()
		defer cancel()
		if err := s.code.waitTurn(ctx, key, ticket); err != nil {
			s.code.update(key, j, func(j *codeJob) {
				j.running = false
				j.status = codeStatus{State: codeFailed, Error: "timeout", Message: "VS Code was being started or stopped on this host for too long"}
			})
			return
		}
		defer s.code.doneTurn(key)
		s.runCode(ctx, key, j, h, f)
	}()
	// Registered first, then checked: access revoked just before is seen here,
	// access revoked from now on cancels the job.
	if _, err := s.auth.CheckLogin(r.Context(), loginHash(r)); err != nil || !s.ownerActive(r.Context(), u.ID) {
		s.code.update(key, j, func(j *codeJob) { j.cancel() })
		s.forgetHostCode(h.OwnerID, h.ID)
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	s.audit(r, "code.start", hostTarget(h), map[string]any{"version": s.cfg.CodeServerVersion})
	writeJSON(w, http.StatusAccepted, first)
}

// POST /api/hosts/{id}/code/stop stops code-server on the host.
func (s *Server) apiCodeStop(w http.ResponseWriter, r *http.Request) {
	h, ok := s.codeHost(w, r)
	if !ok {
		return
	}
	key := [2]int64{h.OwnerID, h.ID}
	f, err := s.store.EnsureCodeForward(r.Context(), h.OwnerID, h.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// One step: the instance to stop (as known now), cancelling a start in
	// progress, and the turn, so a start asked for after this runs after it.
	s.code.mu.Lock()
	target := ""
	if j := s.code.jobs[key]; j != nil {
		target = j.instance
		if j.cancel != nil {
			j.cancel()
		}
		delete(s.code.jobs, key)
	}
	if s.code.stops == nil {
		s.code.stops = map[[2]int64]int{}
	}
	s.code.stops[key]++
	ticket := s.code.enqueue(key)
	s.code.mu.Unlock()
	defer func() {
		s.code.mu.Lock()
		if s.code.stops[key]--; s.code.stops[key] <= 0 {
			delete(s.code.stops, key)
		}
		s.code.mu.Unlock()
	}()
	s.revokeForwardGrants(f.ID, 0) // open VS Code tabs end now
	ctx, cancel := context.WithTimeout(r.Context(), 3*codeStepTimeout+codeCancelTimeout)
	defer cancel()
	if err := s.code.waitTurn(ctx, key, ticket); err != nil {
		writeError(w, http.StatusConflict, "busy", "VS Code is being started or stopped on this host; try again in a moment")
		return
	}
	defer s.code.doneTurn(key)
	if h.Auth != store.AuthAsk && h.CredentialID != nil {
		if err := s.stopCode(ctx, f, target); err != nil {
			writeError(w, http.StatusBadGateway, "code_unreachable", "could not stop VS Code on the host: "+err.Error())
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

var errCodeRevoked = codeFail("revoked", "access to this host changed; open VS Code again")

func (s *Server) runCode(ctx context.Context, key [2]int64, j *codeJob, h *store.Host, f *store.Forward) {
	// Before each phase: the owner's access and the host's connection
	// settings are what they were when the job was asked for.
	still := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.code.gen(h.OwnerID) != j.userGen {
			return errCodeRevoked
		}
		if cur, err := s.store.Host(ctx, h.OwnerID, h.ID); err != nil || cur.ConnVersion != j.version {
			return errCodeRevoked
		}
		return nil
	}
	step := func(state string) error {
		s.code.update(key, j, func(j *codeJob) { j.status = codeStatus{State: state} })
		return still()
	}
	sock, instance, err := s.startCode(ctx, h, f, step, still)
	s.code.update(key, j, func(j *codeJob) {
		j.running = false
		if err == nil {
			j.socket, j.instance = sock, instance
			j.status = codeStatus{State: codeReady, URL: s.cfg.Origin() + "/fwd/" + f.ID}
			return
		}
		var cf *codeFailure
		if !errors.As(err, &cf) {
			cf = &codeFailure{code: "failed", msg: err.Error()}
		}
		if ctx.Err() != nil && cf.code != "revoked" {
			cf = &codeFailure{code: "timeout", msg: "starting VS Code took too long"}
		}
		j.status = codeStatus{State: codeFailed, Error: cf.code, Message: cf.msg}
	})
}

// codeRun runs one of the codeserver commands and returns its answer fields.
func (s *Server) codeRun(ctx context.Context, fc *fwdConn, cmd string, limit time.Duration, opt sshx.ExecOptions) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := sshx.Exec(ctx, fc.client, cmd, sshx.MaxOutput, opt)
	fields := codeserver.Fields(out)
	if err != nil && fields["error"] == "" && fields["result"] == "" {
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
	fields, err := s.codeRun(ctx, fc, codeserver.ProbeCommand(s.cfg.CodeServerVersion), codeStepTimeout, sshx.ExecOptions{})
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
	sock, err := codeserver.SocketPath(p.Home, p.UID, p.Runtime)
	if err != nil {
		return nil, "", codeFail("socket", "%v", err)
	}
	return p, sock, nil
}

// codeTarget is what reaching the host takes, read now: an install that has
// to be ended is ended on the machine and account it ran on, also after the
// host was edited or deleted.
func (s *Server) codeTarget(ctx context.Context, h *store.Host) (*sshx.Target, error) {
	if h.CredentialID == nil {
		return nil, errors.New("no stored credential")
	}
	_, sec, err := s.openCredential(ctx, h.OwnerID, *h.CredentialID)
	if err != nil {
		return nil, err
	}
	k, err := s.store.HostKey(ctx, h.OwnerID, h.ID)
	if err != nil {
		return nil, err
	}
	return &sshx.Target{Address: h.Address, Port: h.Port, User: h.Username, Password: sec.Password, PrivateKey: []byte(sec.PrivateKey),
		Passphrase: []byte(sec.Passphrase), Pinned: &sshx.Pinned{KeyType: k.KeyType, KeyData: k.KeyData, Fingerprint: k.Fingerprint}}, nil
}

func (s *Server) startCode(ctx context.Context, h *store.Host, f *store.Forward, step func(string) error, still func() error) (string, string, error) {
	if err := still(); err != nil {
		return "", "", err
	}
	fc, err := s.codeConn(ctx, f)
	if err != nil {
		return "", "", err
	}
	defer s.fwd.release(fc)
	if err := still(); err != nil { // the connection came from the current settings
		return "", "", err
	}
	p, sock, err := s.probeCode(ctx, fc)
	if err != nil {
		return "", "", err
	}
	ver := s.cfg.CodeServerVersion
	switch {
	case !p.Flock:
		return "", "", codeFail("no_flock", "the host has no flock (util-linux), which VS Code needs")
	case !p.Setsid:
		return "", "", codeFail("no_setsid", "the host has no setsid (util-linux), which VS Code needs")
	}
	if !p.Installed {
		if err := s.installCode(ctx, h, fc, p, step); err != nil {
			return "", "", err
		}
	}
	if err := step(codeStarting); err != nil {
		return "", "", err
	}
	fields, err := s.codeRun(ctx, fc, codeserver.StartCommand(ver, sock, newTaskID()), codeStepTimeout, sshx.ExecOptions{})
	if err != nil {
		return "", "", err
	}
	switch fields["error"] {
	case "socketdir":
		return "", "", codeFail("socket", "the directory for code-server's socket is not a private directory of the account (%s)",
			strings.TrimSpace(fields["detail"]))
	case "verify", "kill":
		return "", "", codeFail("start", "an earlier code-server on the host could not be stopped safely (%s)", strings.TrimSpace(fields["detail"]))
	case "nosetsid":
		return "", "", codeFail("no_setsid", "code-server did not get a process group of its own on the host")
	}
	if r := fields["result"]; r != "started" && r != "running" {
		return "", "", codeFail("start", "code-server did not start (%s %s); its log is ~/.local/state/corral/code-server.log on the host",
			fields["error"], strings.TrimSpace(fields["detail"]))
	}
	s.fwd.setCodeSocket(fc, sock)
	if err := s.codeHealthy(ctx, fc); err != nil {
		return "", "", codeFail("start", "code-server does not answer on its socket (%v); its log is ~/.local/state/corral/code-server.log on the host", err)
	}
	if err := still(); err != nil {
		return "", "", err
	}
	return sock, fields["instance"], nil
}

// installCode installs the release in one command on the host (see
// codeserver.InstallCommand): downloaded there, or the gateway's copy sent
// on its input. Its channel counts against the forwarding channel quota until
// it has really ended. If the command's outcome is unknown (cancelled, timed
// out, connection lost), the install is ended on the host before returning.
func (s *Server) installCode(ctx context.Context, h *store.Host, fc *fwdConn, p *codeserver.Probe, step func(string) error) error {
	ver, sum := s.cfg.CodeServerVersion, s.cfg.CodeServerSHA256[p.Arch]
	switch {
	case sum == "":
		return codeFail("no_checksum", "no trusted SHA-256 digest is configured for code-server %s on %s (CORRAL_CODE_SERVER_SHA256)", ver, p.Arch)
	case !p.Tar:
		return codeFail("no_tar", "the host has no tar, which unpacking code-server needs")
	case p.FreeKB >= 0 && p.FreeKB < codeserver.MinFreeKB:
		return codeFail("no_space", "only %d MB are free in the home directory; installing VS Code needs about %d MB",
			p.FreeKB>>10, codeserver.MinFreeKB>>10)
	}
	from, url := codeserver.FromDownload, s.cfg.CodeServerDownload(p.Arch)
	var input io.Reader
	if s.cfg.CodeServerDir != "" {
		name := s.cfg.CodeServerFile(p.Arch)
		path := filepath.Join(s.cfg.CodeServerDir, name)
		if got, err := fileSHA256(path); err != nil {
			return codeFail("archive_missing", "%s is not in CORRAL_CODE_SERVER_DIR on the gateway", name)
		} else if got != sum {
			return codeFail("checksum", "%s in CORRAL_CODE_SERVER_DIR does not match its trusted SHA-256 digest", name)
		}
		src, err := os.Open(path)
		if err != nil {
			return codeFail("archive_missing", "%s is not in CORRAL_CODE_SERVER_DIR on the gateway", name)
		}
		defer src.Close()
		from, url, input = codeserver.FromStdin, "", src
		if err := step(codeUploading); err != nil {
			return err
		}
	} else {
		if !p.Curl && !p.Wget {
			return codeFail("no_downloader", "the host has neither curl nor wget to download code-server; "+
				"put the release in CORRAL_CODE_SERVER_DIR on the gateway to upload it instead")
		}
		if err := step(codeDownloading); err != nil {
			return err
		}
	}
	target, err := s.codeTarget(ctx, h)
	if err != nil {
		return codeFail("connect", "could not read how to reach the host: %v", err)
	}
	release, ok := s.fwd.takeChannel(fc.client)
	if !ok {
		return codeFail("busy", "too many forwarding connections are open; try again in a moment")
	}
	task := newTaskID()
	phases := map[string]string{"downloading": codeDownloading, "uploading": codeUploading, "checking": codeExtracting, "extracting": codeExtracting}
	fields, err := s.codeRun(ctx, fc, codeserver.InstallCommand(task, ver, p.Arch, from, url, sum), codeInstallTimeout, sshx.ExecOptions{
		Stdin: input, Release: release,
		OnLine: func(line string) {
			if ph, ok := phases[strings.TrimPrefix(line, "phase=")]; ok && strings.HasPrefix(line, "phase=") {
				_ = step(ph)
			}
		},
	})
	if err != nil || (fields["result"] == "" && fields["error"] == "") || ctx.Err() != nil {
		s.cancelInstall(target, task)
		if err == nil {
			err = codeFail("timeout", "the install did not finish")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	switch fields["error"] {
	case "":
	case "download":
		return codeFail("download", "the host could not download code-server (%s); if it has no internet access, "+
			"put %s in CORRAL_CODE_SERVER_DIR on the gateway to upload it instead",
			strings.TrimSpace(fields["detail"]), s.cfg.CodeServerFile(p.Arch))
	case "nodownloader":
		return codeFail("no_downloader", "the host has neither curl nor wget to download code-server")
	case "checksum":
		return codeFail("checksum", "the code-server archive on the host does not match its trusted SHA-256 digest (got %s); it was removed",
			strings.TrimSpace(fields["detail"]))
	case "nosha":
		return codeFail("no_sha", "the host has neither sha256sum nor shasum to check the code-server archive")
	case "noflock":
		return codeFail("no_flock", "the host has no flock (util-linux), which VS Code needs")
	case "nosetsid":
		return codeFail("no_setsid", "the host has no setsid (util-linux), which VS Code needs")
	case "members", "layout":
		return codeFail("archive", "the code-server archive holds unexpected files (%s); nothing was installed", fields["error"])
	case "locked":
		return codeFail("busy", "another install or start of VS Code is running on this host account; try again in a moment")
	default:
		return codeFail("extract", "installing code-server failed (%s %s)", fields["error"], strings.TrimSpace(fields["detail"]))
	}
	if fields["result"] != "ok" {
		return codeFail("extract", "installing code-server failed")
	}
	return nil
}

// cancelInstall ends an install whose outcome is unknown, on a connection of
// its own made with the settings the install ran with, and waits for it.
func (s *Server) cancelInstall(t *sshx.Target, task string) {
	ctx, cancel := context.WithTimeout(context.Background(), codeCancelTimeout)
	defer cancel()
	dialed, err := sshx.Dial(ctx, s.guard, *t)
	if err != nil {
		s.log.Warn("could not reach the host to end a cancelled VS Code install", "err", err)
		return
	}
	defer dialed.Client.Close()
	fields, err := s.codeRun(ctx, &fwdConn{client: dialed.Client}, codeserver.CancelInstallCommand(task), codeCancelTimeout, sshx.ExecOptions{})
	if err != nil || fields["result"] != "ok" {
		s.log.Warn("could not end a cancelled VS Code install on the host", "err", err, "answer", fields["error"])
	}
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

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// newTaskID names one install, or one start of code-server in its pid file.
func newTaskID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// stopCode stops code-server on the host (also one started before a restart
// of the gateway). With target, only that instance.
func (s *Server) stopCode(ctx context.Context, f *store.Forward, target string) error {
	fc, err := s.codeConn(ctx, f)
	if err != nil {
		return err
	}
	defer s.fwd.release(fc)
	return s.stopCodeOn(ctx, fc, target)
}

// codeStopperFor prepares stopping VS Code on a host that is about to be
// deleted: what is needed to reach it is read now, the stop runs in the
// background afterwards and never holds up the delete. nil when there is
// nothing to stop or no saved way in.
func (s *Server) codeStopperFor(ctx context.Context, h *store.Host) func() {
	if !s.cfg.CodeServer || h.Auth == store.AuthAsk || h.CredentialID == nil || !s.store.HasCodeForward(ctx, h.OwnerID, h.ID) {
		return nil
	}
	t, err := s.codeTarget(ctx, h)
	if err != nil {
		return nil
	}
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
			dialed, err := sshx.Dial(ctx, s.guard, *t)
			if err != nil {
				return
			}
			defer dialed.Client.Close()
			if err := s.stopCodeOn(ctx, &fwdConn{client: dialed.Client}, ""); err != nil {
				s.log.Info("could not stop VS Code on a deleted host", "host", h.Name, "err", err)
			}
		}()
	}
}

func (s *Server) stopCodeOn(ctx context.Context, fc *fwdConn, target string) error {
	_, sock, err := s.probeCode(ctx, fc)
	if err != nil {
		return err
	}
	fields, err := s.codeRun(ctx, fc, codeserver.StopCommand(sock, target), codeStepTimeout, sshx.ExecOptions{})
	if err != nil {
		return err
	}
	switch {
	case fields["error"] == "verify":
		return errors.New("its pid file could not be read, so nothing was stopped")
	case fields["error"] == "kill":
		return errors.New("code-server is still running after being told to stop")
	case fields["error"] == "locked":
		return errors.New("an install or start of VS Code is still running on this host account")
	case fields["result"] == "other":
		return nil // a newer instance runs; it is not this stop's to end
	case fields["result"] != "ok":
		return fmt.Errorf("the stop did not finish (%s)", fields["error"])
	}
	return nil
}
