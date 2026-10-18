package web

// VS Code in the browser: code-server on the host, installed and started on
// demand over a forwarding connection of its own, and served like a private
// forward on "code-<id>.<forward domain>" (same tickets, cookie, per-request
// checks and revocation as forwarded ports).
//
// Starting is a background job the browser polls; there is at most one per
// owner and host. Its state lives in memory: after a gateway restart the
// next start finds code-server still running and reuses it.
//
// Starts and stops of one host take turns in the order they were asked for
// (a ticket queue, bounded per host and per user). A turn passes on only
// once the host has confirmed that its operation ended (or never ran). When
// that cannot be confirmed (host unreachable, no answer in time), the host
// is marked as needing a cleanup: every later start or stop first settles
// that operation on the host, and is refused until it can. On the host,
// installing, starting and stopping also hold a lock, and each operation has
// an id that a settle can claim so it never runs late.
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

	"golang.org/x/crypto/ssh"

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
	codeCleaningUp  = "cleanup" // an earlier operation's end could not be confirmed
)

// Limits and timeouts; variables so tests can lower them.
var (
	maxCodeJobsPerUser  = 3                // starts working at once, per user
	maxCodeOpsPerHost   = 4                // starts and stops asked for and not yet ended, per host
	maxCodeOpsPerUser   = 8                // the same, per user
	codeStepTimeout     = 2 * time.Minute  // checking, starting, stopping
	codeInstallTimeout  = 15 * time.Minute // downloading or uploading, checking and unpacking the release
	codeCancelTimeout   = time.Minute      // ending or settling an operation whose end is unknown
	codeHealthAttempts  = 20
	codeHealthInterval  = 500 * time.Millisecond
	codeHealthPerTryMax = 5 * time.Second
)

// Tests only: codeHookBeforeRegister runs between a start's access check and
// its registration, codeHookAfterStart once the host has answered the start.
var codeHookBeforeRegister, codeHookAfterStart func()

type codeStatus struct {
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`   // failure code, for the interface
	Message string `json:"message,omitempty"` // failure details
	URL     string `json:"url,omitempty"`     // when ready: opens VS Code (ticket hand-off)
}

type codeJob struct {
	status   codeStatus
	socket   string // where code-server listens, once ready (or still, see socket)
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

// codeCleanup is an operation on the host whose end was not confirmed: an
// install to cancel, or a start or stop to settle. target is how the
// operation reached the host (also after the host was edited).
type codeCleanup struct {
	target  *sshx.Target
	install string // the install's task id, or
	op      string // the start's or stop's operation id
	err     string // why the last attempt failed
}

type codeServers struct {
	mu      sync.Mutex
	jobs    map[[2]int64]*codeJob // by owner and host
	stops   map[[2]int64]int      // stops queued or running
	queues  map[[2]int64]*codeQueue
	ops     map[[2]int64]int          // turns taken and not yet ended, per host
	userOps map[int64]int             // the same, per user
	cleanup map[[2]int64]*codeCleanup // unconfirmed operations, per host
	userGen map[int64]uint64          // bumped when a user's access is revoked (disable, admin reset)
	closed  bool
	wg      sync.WaitGroup
}

func (c *codeServers) status(owner, host int64) codeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := [2]int64{owner, host}
	j := c.jobs[key]
	switch {
	case j != nil && j.running:
		return j.status
	case c.stops[key] > 0:
		return codeStatus{State: codeStopping}
	case c.cleanup[key] != nil:
		st := codeStatus{State: codeCleaningUp, Error: "cleanup_pending", Message: "Corral could not confirm that an earlier VS Code " +
			"operation on this host ended (" + c.cleanup[key].err + "); it checks again before the next start or stop"}
		if j != nil && j.status.State == codeFailed {
			st.State = codeFailed // a start that has just failed says so (and why it matters)
		}
		return st
	case j != nil:
		return j.status
	}
	return codeStatus{State: codeStopped}
}

// socket is the code-server socket of a ready instance, or "". A start asked
// for while an instance is ready (VS Code opened again, e.g. on a second
// device) begins with that instance's socket, so the tabs open on it keep
// working while the start checks it again; a check that fails ends that.
func (c *codeServers) socket(owner, host int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.jobs[[2]int64{owner, host}]; j != nil && (j.status.State == codeReady || j.running) {
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

var errCodeQueueFull = errors.New("too many VS Code starts and stops are waiting")

// enqueue takes the next turn for a host; called with mu held, together with
// registering what the turn is for, so the order of requests is the order
// of turns. The turn counts against the host's and the user's limits until
// it ends (doneTurn) or is given up (waitTurn, abandon).
func (c *codeServers) enqueue(key [2]int64) (uint64, error) {
	if c.ops[key] >= maxCodeOpsPerHost || c.userOps[key[0]] >= maxCodeOpsPerUser {
		return 0, errCodeQueueFull
	}
	if c.queues == nil {
		c.queues, c.ops, c.userOps = map[[2]int64]*codeQueue{}, map[[2]int64]int{}, map[int64]int{}
	}
	q := c.queues[key]
	if q == nil {
		q = &codeQueue{abandoned: map[uint64]bool{}, changed: make(chan struct{})}
		c.queues[key] = q
	}
	c.ops[key]++
	c.userOps[key[0]]++
	t := q.next
	q.next++
	return t, nil
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
			c.abandonLocked(key, t)
			c.mu.Unlock()
			return ctx.Err()
		}
	}
}

// abandonLocked gives up ticket t, served or not.
func (c *codeServers) abandonLocked(key [2]int64, t uint64) {
	q := c.queues[key]
	if q.serving == t {
		c.advance(key, q)
	} else {
		q.abandoned[t] = true
		c.uncount(key)
	}
}

// doneTurn ends the turn being served.
func (c *codeServers) doneTurn(key [2]int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(key, c.queues[key])
}

func (c *codeServers) uncount(key [2]int64) {
	if c.ops[key]--; c.ops[key] <= 0 {
		delete(c.ops, key)
	}
	if c.userOps[key[0]]--; c.userOps[key[0]] <= 0 {
		delete(c.userOps, key[0])
	}
}

func (c *codeServers) advance(key [2]int64, q *codeQueue) {
	c.uncount(key)
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
// or its connection settings changed), cancelling a start in progress. A
// cleanup still to be done stays: it is done with the old settings.
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
	if codeHookBeforeRegister != nil {
		codeHookBeforeRegister()
	}
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
	ticket, err := s.code.enqueue(key) // with the registration: after any stop asked for earlier
	if err != nil {
		s.code.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "busy", "VS Code is being started or stopped too often on this host; try again in a moment")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*codeInstallTimeout+3*codeStepTimeout+3*codeCancelTimeout)
	j := &codeJob{status: codeStatus{State: codeChecking}, running: true, cancel: cancel, userGen: s.code.userGen[h.OwnerID],
		version: h.ConnVersion}
	if old := s.code.jobs[key]; old != nil && old.status.State == codeReady {
		j.socket = old.socket // see socket; its instance is known again once the start confirms it
	}
	s.code.jobs[key] = j
	first := j.status
	s.code.wg.Add(1)
	s.code.mu.Unlock()
	// Registered first, then checked, and only then started: access revoked
	// before this point is seen here; revoked after it, it cancels the job.
	if !s.codeAccessHolds(r, u, h, j) {
		cancel()
		s.code.mu.Lock()
		if s.code.jobs[key] == j {
			delete(s.code.jobs, key)
		}
		s.code.abandonLocked(key, ticket)
		s.code.mu.Unlock()
		s.code.wg.Done()
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
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
	s.audit(r, "code.start", hostTarget(h), map[string]any{"version": s.cfg.CodeServerVersion})
	writeJSON(w, http.StatusAccepted, first)
}

// codeAccessHolds re-checks, for a job just registered, what the request was
// let in with: the login, the owner's account and revocation generation, and
// the host's connection settings.
func (s *Server) codeAccessHolds(r *http.Request, u *store.User, h *store.Host, j *codeJob) bool {
	if _, err := s.auth.CheckLogin(r.Context(), loginHash(r)); err != nil || !s.ownerActive(r.Context(), u.ID) {
		return false
	}
	if s.code.gen(h.OwnerID) != j.userGen {
		return false
	}
	cur, err := s.store.Host(r.Context(), h.OwnerID, h.ID)
	return err == nil && cur.ConnVersion == j.version
}

// POST /api/hosts/{id}/code/stop stops code-server on the host. The stop
// runs in the background, in its turn; the answer waits for it unless the
// client goes away first.
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
	// One step: the turn, the instance to stop (when known now), and
	// cancelling a start in progress, so a start asked for after this runs
	// after it.
	s.code.mu.Lock()
	if s.code.closed {
		s.code.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "busy", "the server is shutting down")
		return
	}
	ticket, err := s.code.enqueue(key)
	if err != nil {
		s.code.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "busy", "VS Code is being started or stopped too often on this host; try again in a moment")
		return
	}
	target := ""
	if j := s.code.jobs[key]; j != nil {
		if j.status.State == codeReady {
			target = j.instance
		}
		if j.cancel != nil {
			j.cancel()
		}
		delete(s.code.jobs, key)
	}
	if s.code.stops == nil {
		s.code.stops = map[[2]int64]int{}
	}
	s.code.stops[key]++
	s.code.wg.Add(1)
	s.code.mu.Unlock()
	s.revokeForwardGrants(f.ID, 0) // open VS Code tabs end now
	done := make(chan error, 1)
	go func() {
		defer s.code.wg.Done()
		defer func() {
			s.code.mu.Lock()
			if s.code.stops[key]--; s.code.stops[key] <= 0 {
				delete(s.code.stops, key)
			}
			s.code.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*codeStepTimeout+3*codeCancelTimeout)
		defer cancel()
		if err := s.code.waitTurn(ctx, key, ticket); err != nil {
			done <- codeFail("busy", "VS Code was being started or stopped on this host for too long")
			return
		}
		defer s.code.doneTurn(key)
		if err := s.reconcileCode(key); err != nil {
			done <- err
			return
		}
		if h.Auth == store.AuthAsk || h.CredentialID == nil {
			done <- nil // no way in: nothing Corral can stop
			return
		}
		done <- s.stopCode(ctx, key, h, f, target)
	}()
	select {
	case err := <-done:
		var cf *codeFailure
		switch {
		case err == nil:
			s.audit(r, "code.stop", hostTarget(h), nil)
			writeJSON(w, http.StatusOK, codeStatus{State: codeStopped})
		case errors.As(err, &cf) && cf.code == "cleanup_pending":
			writeError(w, http.StatusConflict, "cleanup_pending", cf.msg)
		case errors.As(err, &cf) && cf.code == "busy":
			writeError(w, http.StatusConflict, "busy", cf.msg)
		default:
			writeError(w, http.StatusBadGateway, "code_unreachable", "could not stop VS Code on the host: "+err.Error())
		}
	case <-r.Context().Done(): // the stop carries on
	}
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
	var sock, instance string
	err := s.reconcileCode(key) // an earlier operation's end first
	if err == nil {
		sock, instance, err = s.startCode(ctx, key, h, f, step, still)
	}
	s.code.update(key, j, func(j *codeJob) {
		j.running = false
		if err == nil {
			j.socket, j.instance = sock, instance
			j.status = codeStatus{State: codeReady, URL: s.cfg.Origin() + "/fwd/" + f.ID}
			return
		}
		j.socket = "" // an instance it began with did not pass the check either
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

// codeRun runs one of the codeserver commands over c and returns its answer
// fields, and whether the command is known to have ended on the host. When
// it is not, the operation may still run there: the caller settles it.
func (s *Server) codeRun(ctx context.Context, c *ssh.Client, cmd string, limit time.Duration, opt sshx.ExecOptions) (map[string]string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	r, err := sshx.Exec(ctx, c, cmd, sshx.MaxOutput, opt)
	fields := codeserver.Fields(r.Out)
	switch {
	case errors.Is(err, sshx.ErrBusy): // nothing was started
		return nil, true, codeFail("busy", "earlier commands on this host are still running; try again in a moment")
	case !r.Ended && ctx.Err() != nil:
		return fields, false, codeFail("timeout", "the host did not answer in time")
	case !r.Ended:
		return fields, false, codeFail("failed", "a command on the host failed: %v", err)
	case fields["error"] == "" && fields["result"] == "":
		return fields, true, codeFail("failed", "a command on the host gave no answer: %v", err)
	}
	return fields, true, nil
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
func (s *Server) probeCode(ctx context.Context, c *ssh.Client) (*codeserver.Probe, string, error) {
	fields, _, err := s.codeRun(ctx, c, codeserver.ProbeCommand(s.cfg.CodeServerVersion), codeStepTimeout, sshx.ExecOptions{})
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

// codeTarget is what reaching the host takes, read now: an operation whose
// end is unknown is settled on the machine and account it ran on, also
// after the host was edited or deleted.
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

// Cleanups ---------------------------------------------------------------------

// runCleanup ends or settles an operation on the host, over a connection of
// its own; nil when the host confirmed it.
func (s *Server) runCleanup(cl *codeCleanup) error {
	ctx, cancel := context.WithTimeout(context.Background(), codeCancelTimeout)
	defer cancel()
	dialed, err := sshx.Dial(ctx, s.guard, *cl.target)
	if err != nil {
		return fmt.Errorf("could not reach the host: %w", err)
	}
	defer dialed.Client.Close()
	var fields map[string]string
	var ended bool
	if cl.install != "" {
		fields, ended, err = s.codeRun(ctx, dialed.Client, codeserver.CancelInstallCommand(cl.install), codeCancelTimeout, sshx.ExecOptions{})
	} else {
		fields, ended, err = s.codeRun(ctx, dialed.Client, codeserver.SettleCommand(cl.op), codeCancelTimeout, sshx.ExecOptions{})
	}
	switch {
	case err != nil:
		return err
	case !ended || fields["result"] != "ok":
		return fmt.Errorf("the host answered %q", fields["error"])
	}
	return nil
}

// settle makes sure an operation whose end is unknown is over on the host.
// If the host cannot confirm it now, the host's next start or stop has to
// (cleanup pending).
func (s *Server) settle(key [2]int64, cl *codeCleanup) {
	if err := s.runCleanup(cl); err != nil {
		s.log.Warn("could not confirm that a VS Code operation on the host ended", "err", err)
		s.code.mu.Lock()
		if s.code.cleanup == nil {
			s.code.cleanup = map[[2]int64]*codeCleanup{}
		}
		cl.err = err.Error()
		s.code.cleanup[key] = cl
		s.code.mu.Unlock()
	}
}

// reconcileCode comes first in every turn: a cleanup still to be done must
// succeed before anything else runs on the host.
func (s *Server) reconcileCode(key [2]int64) error {
	s.code.mu.Lock()
	cl := s.code.cleanup[key]
	s.code.mu.Unlock()
	if cl == nil {
		return nil
	}
	if err := s.runCleanup(cl); err != nil {
		s.code.mu.Lock()
		cl.err = err.Error()
		s.code.mu.Unlock()
		return codeFail("cleanup_pending", "Corral could not confirm that an earlier VS Code operation on this host ended (%v); "+
			"try again once the host is reachable", err)
	}
	s.code.mu.Lock()
	if s.code.cleanup[key] == cl {
		delete(s.code.cleanup, key)
	}
	s.code.mu.Unlock()
	return nil
}

// Starting -------------------------------------------------------------------

func (s *Server) startCode(ctx context.Context, key [2]int64, h *store.Host, f *store.Forward, step func(string) error,
	still func() error) (string, string, error) {
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
	target, err := s.codeTarget(ctx, h)
	if err != nil {
		return "", "", codeFail("connect", "could not read how to reach the host: %v", err)
	}
	p, sock, err := s.probeCode(ctx, fc.client)
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
		if err := s.installCode(ctx, key, target, p, step); err != nil {
			return "", "", err
		}
	}
	if err := step(codeStarting); err != nil {
		return "", "", err
	}
	instance, op := newTaskID(), newTaskID()
	fields, ended, err := s.codeRun(ctx, fc.client, codeserver.StartCommand(ver, sock, instance, op), codeStepTimeout, sshx.ExecOptions{})
	if !ended {
		s.settle(key, &codeCleanup{target: target, op: op})
		if err == nil {
			err = codeFail("timeout", "the start did not finish")
		}
		return "", "", err
	}
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
	case "locked":
		return "", "", codeFail("busy", "another install, start or stop of VS Code is running on this host account; try again in a moment")
	}
	if r := fields["result"]; r != "started" && r != "running" {
		return "", "", codeFail("start", "code-server did not start (%s %s); its log is ~/.local/state/corral/code-server.log on the host",
			fields["error"], strings.TrimSpace(fields["detail"]))
	}
	if fields["result"] == "running" {
		instance = fields["instance"]
	}
	if codeHookAfterStart != nil {
		codeHookAfterStart()
	}
	// Revoked meanwhile: an instance this job started itself is stopped again.
	if err := still(); err != nil {
		if fields["result"] == "started" {
			s.stopStarted(key, target, fc.client, sock, instance)
		}
		return "", "", err
	}
	s.fwd.setCodeSocket(fc, sock)
	if err := s.codeHealthy(ctx, fc); err != nil {
		return "", "", codeFail("start", "code-server does not answer on its socket (%v); its log is ~/.local/state/corral/code-server.log on the host", err)
	}
	if err := still(); err != nil {
		if fields["result"] == "started" {
			s.stopStarted(key, target, fc.client, sock, instance)
		}
		return "", "", err
	}
	return sock, instance, nil
}

// stopStarted stops the instance a job started when the job's access was
// revoked before it was ready.
func (s *Server) stopStarted(key [2]int64, target *sshx.Target, c *ssh.Client, sock, instance string) {
	ctx, cancel := context.WithTimeout(context.Background(), codeStepTimeout)
	defer cancel()
	op := newTaskID()
	if _, ended, _ := s.codeRun(ctx, c, codeserver.StopCommand(sock, instance, op), codeStepTimeout, sshx.ExecOptions{}); !ended {
		s.settle(key, &codeCleanup{target: target, op: op})
	}
}

// installCode installs the release in one command on the host (see
// codeserver.InstallCommand): downloaded there, or the gateway's copy sent
// on its input. It runs over a connection of its own, bounded in time and
// closed when the time is up. If the command's end is unknown (cancelled,
// timed out, connection lost), the install is ended on the host before
// returning, or the host is marked for a cleanup.
func (s *Server) installCode(ctx context.Context, key [2]int64, target *sshx.Target, p *codeserver.Probe, step func(string) error) error {
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
	ictx, cancel := context.WithTimeout(ctx, codeInstallTimeout)
	defer cancel()
	dialed, err := sshx.Dial(ictx, s.guard, *target)
	if err != nil {
		return codeFail("connect", "could not connect to the host: %v", err)
	}
	defer dialed.Client.Close()
	// Time up or cancelled: the connection goes, and with it anything still
	// copying the input or waiting for the host.
	stop := context.AfterFunc(ictx, func() { dialed.Client.Close() })
	defer stop()
	task := newTaskID()
	phases := map[string]string{"downloading": codeDownloading, "uploading": codeUploading, "checking": codeExtracting, "extracting": codeExtracting}
	fields, ended, err := s.codeRun(ictx, dialed.Client, codeserver.InstallCommand(task, ver, p.Arch, from, url, sum), codeInstallTimeout, sshx.ExecOptions{
		Stdin: input,
		OnLine: func(line string) {
			if ph, ok := phases[strings.TrimPrefix(line, "phase=")]; ok && strings.HasPrefix(line, "phase=") {
				_ = step(ph)
			}
		},
	})
	if !ended {
		dialed.Client.Close()
		s.settle(key, &codeCleanup{target: target, install: task})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = codeFail("timeout", "the install did not finish")
		}
		return err
	}
	if err != nil {
		return err
	}
	switch fields["error"] {
	case "":
	case "download":
		return codeFail("download", "the host could not download code-server (%s); if it has no internet access, "+
			"put %s in CORRAL_CODE_SERVER_DIR on the gateway to upload it instead",
			strings.TrimSpace(fields["detail"]), s.cfg.CodeServerFile(p.Arch))
	case "nodownloader":
		return codeFail("no_downloader", "the host has neither curl nor wget (with timeout) to download code-server")
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
		return codeFail("archive", "the code-server archive holds unexpected files (%s %s); nothing was installed",
			fields["error"], strings.TrimSpace(fields["detail"]))
	case "locked":
		return codeFail("busy", "another install, start or stop of VS Code is running on this host account; try again in a moment")
	case "cancelled":
		return codeFail("timeout", "the install was cancelled")
	default:
		return codeFail("extract", "installing code-server failed (%s %s)", fields["error"], strings.TrimSpace(fields["detail"]))
	}
	if fields["result"] != "ok" {
		return codeFail("extract", "installing code-server failed")
	}
	return nil
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

// newTaskID names one install, operation, or instance of code-server.
func newTaskID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Stopping -------------------------------------------------------------------

// stopCode stops instance target on the host. Without a target (nothing
// known here, as after a gateway restart, or a start still under way) it
// stops the instance the host names now, and only that one.
func (s *Server) stopCode(ctx context.Context, key [2]int64, h *store.Host, f *store.Forward, target string) error {
	fc, err := s.codeConn(ctx, f)
	if err != nil {
		return err
	}
	defer s.fwd.release(fc)
	t, err := s.codeTarget(ctx, h)
	if err != nil {
		return codeFail("connect", "could not read how to reach the host: %v", err)
	}
	_, sock, err := s.probeCode(ctx, fc.client)
	if err != nil {
		return err
	}
	if target == "" {
		fields, _, err := s.codeRun(ctx, fc.client, codeserver.InstanceCommand(), codeStepTimeout, sshx.ExecOptions{})
		switch {
		case err != nil:
			return err
		case fields["error"] == "verify":
			return errors.New("its pid file could not be verified, so nothing was stopped")
		case fields["result"] != "ok":
			return fmt.Errorf("the host did not say what runs (%s)", fields["error"])
		case fields["instance"] == "":
			return nil // nothing runs
		case !codeserver.ValidTarget(fields["instance"]):
			return errors.New("the host named an unusable instance")
		}
		target = fields["instance"]
	}
	op := newTaskID()
	fields, ended, err := s.codeRun(ctx, fc.client, codeserver.StopCommand(sock, target, op), codeStepTimeout, sshx.ExecOptions{})
	if !ended {
		s.settle(key, &codeCleanup{target: t, op: op})
		if err == nil {
			err = errors.New("the stop did not finish")
		}
		return err
	}
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

// codeStopperFor prepares stopping VS Code on a host that is about to be
// deleted: what is needed to reach it is read now, the stop runs in the
// background afterwards and never holds up the delete. A cleanup still to
// be done is done first. Whatever runs is stopped (the force path: nothing
// on a deleted host is to be kept). nil when there is nothing to do or no
// saved way in.
func (s *Server) codeStopperFor(ctx context.Context, h *store.Host) func() {
	key := [2]int64{h.OwnerID, h.ID}
	s.code.mu.Lock()
	cl := s.code.cleanup[key]
	delete(s.code.cleanup, key)
	s.code.mu.Unlock()
	var t *sshx.Target
	if s.cfg.CodeServer && h.Auth != store.AuthAsk && h.CredentialID != nil && s.store.HasCodeForward(ctx, h.OwnerID, h.ID) {
		t, _ = s.codeTarget(ctx, h)
	}
	if cl == nil && t == nil {
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
			if cl != nil {
				if err := s.runCleanup(cl); err != nil {
					s.log.Warn("could not confirm that a VS Code operation on a deleted host ended", "host", h.Name, "err", err)
				}
			}
			if t == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), codeStepTimeout)
			defer cancel()
			dialed, err := sshx.Dial(ctx, s.guard, *t)
			if err != nil {
				return
			}
			defer dialed.Client.Close()
			_, sock, err := s.probeCode(ctx, dialed.Client)
			if err == nil {
				var fields map[string]string
				fields, _, err = s.codeRun(ctx, dialed.Client, codeserver.ForceStopCommand(sock, newTaskID()), codeStepTimeout, sshx.ExecOptions{})
				if err == nil && fields["result"] != "ok" {
					err = fmt.Errorf("the host answered %q", fields["error"])
				}
			}
			if err != nil {
				s.log.Info("could not stop VS Code on a deleted host", "host", h.Name, "err", err)
			}
		}()
	}
}
