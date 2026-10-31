package web

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

// Opening again, after a restart, the tmux and herdr sessions people had
// open (their records: see tabs.go), attached only: a session no longer at
// the host is not made anew. Each record is checked again first (still
// fresh, its user still signed in somewhere, its host as it was); the host
// is reached with what is stored only (a key pinned; never a password
// asked), and asked whether the session is still there. A record taken
// over (opened again from a browser meanwhile) is left. Records of one host
// one after another; four hosts at once, two per user (out of the four
// opens a user may have connecting).

const (
	restoreFresh   = time.Hour        // records older are not opened again
	restoreTimeout = 30 * time.Second // each, from waiting for the host's turn to started
	restoreWorkers = 4
	restorePerUser = 2
	RestoreMax     = 2000 // records read at a start (the newest)
)

type restorer struct {
	mu      sync.Mutex
	ctx     context.Context // ends when the gateway stops
	stop    context.CancelFunc
	stopped bool
	queue   map[int64][][]*store.OpenTab // per user, groups waiting (one host each)
	users   []int64                      // users with groups waiting, taken in turn
	next    int
	running int
	perUser map[int64]int
	cancels map[[2]int64]context.CancelFunc // the group running, per (user, host)
	wg      sync.WaitGroup
}

// StartRestore opens again the records read at the start (the newest
// first, as store.OpenTabs gives them).
func (s *Server) StartRestore(tabs []*store.OpenTab) {
	rs := &s.restores
	rs.mu.Lock()
	if rs.queue == nil {
		rs.queue, rs.perUser, rs.cancels = map[int64][][]*store.OpenTab{}, map[int64]int{}, map[[2]int64]context.CancelFunc{}
	}
	at := map[[2]int64]int{} // (user, host) → its group's index in the user's queue
	for _, t := range tabs {
		k := [2]int64{t.OwnerID, t.HostID}
		i, ok := at[k]
		if !ok {
			if len(rs.queue[t.OwnerID]) == 0 {
				rs.users = append(rs.users, t.OwnerID)
			}
			i = len(rs.queue[t.OwnerID])
			at[k] = i
			rs.queue[t.OwnerID] = append(rs.queue[t.OwnerID], nil)
		}
		rs.queue[t.OwnerID][i] = append(rs.queue[t.OwnerID][i], t)
	}
	rs.mu.Unlock()
	if len(tabs) > 0 {
		s.log.Info("opening again the sessions open before the restart", "sessions", len(tabs))
	}
	s.pumpRestores()
}

// pumpRestores starts groups while there is room: users in turn, each up
// to restorePerUser.
func (s *Server) pumpRestores() {
	rs := &s.restores
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for !rs.stopped && rs.running < restoreWorkers {
		var g []*store.OpenTab
		for tried := 0; tried < len(rs.users) && g == nil; tried++ {
			if rs.next >= len(rs.users) {
				rs.next = 0
			}
			u := rs.users[rs.next]
			if rs.perUser[u] < restorePerUser {
				g, rs.queue[u] = rs.queue[u][0], rs.queue[u][1:]
				if len(rs.queue[u]) == 0 {
					delete(rs.queue, u)
					rs.users = slices.Delete(rs.users, rs.next, rs.next+1)
					continue // (the next user is at this index now)
				}
			}
			rs.next++
		}
		if g == nil {
			return
		}
		owner, key := g[0].OwnerID, [2]int64{g[0].OwnerID, g[0].HostID}
		ctx, cancel := context.WithCancel(rs.ctx)
		rs.cancels[key] = cancel
		rs.running++
		rs.perUser[owner]++
		rs.wg.Add(1)
		go func() {
			defer rs.wg.Done()
			for _, t := range g {
				if ctx.Err() != nil {
					break // (cancelled: what is left stays recorded, for the next start or for nothing)
				}
				s.restoreOne(ctx, t)
			}
			cancel()
			rs.mu.Lock()
			delete(rs.cancels, key)
			rs.running--
			if rs.perUser[owner]--; rs.perUser[owner] <= 0 {
				delete(rs.perUser, owner)
			}
			rs.mu.Unlock()
			s.pumpRestores()
		}()
	}
}

// cancelRestores cancels the user's restores under way (of one host, or
// all with host 0): access revoked, or the host deleted.
func (s *Server) cancelRestores(owner, host int64) {
	rs := &s.restores
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for k, cancel := range rs.cancels {
		if k[0] == owner && (host == 0 || k[1] == host) {
			cancel()
		}
	}
}

// stopRestores cancels every restore, none starts after.
func (s *Server) stopRestores() {
	rs := &s.restores
	rs.mu.Lock()
	rs.stopped = true
	rs.mu.Unlock()
	rs.stop()
}

// WaitRestores waits for the restores under way to end (after BeginShutdown).
func (s *Server) WaitRestores(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		s.restores.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

var (
	errHostChanged = errors.New("the host was changed")
	errRestoreDB   = errors.New("database")
)

// restoreOne opens one record again, or says why not (its outcome, audited
// as session.restore).
func (s *Server) restoreOne(parent context.Context, t *store.OpenTab) {
	ctx, cancel := context.WithTimeout(parent, restoreTimeout)
	defer cancel()
	out := restoreOutcome{t: t}
	defer func() { s.auditRestore(&out) }()
	drop := func(why string) {
		out.outcome = why
		dctx, dcancel := s.endCtx()
		defer dcancel()
		if err := s.store.DeleteTab(dctx, t.OwnerID, t.SessionID); err != nil {
			s.log.Warn("open tabs: a record not removed", "session", t.SessionID, "err", err)
		}
	}
	keep := func(why string) { out.outcome = why }
	gone := func() string { // why ctx ended
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
			return "cancelled:timeout"
		}
		if s.terms.Closing() {
			return "cancelled:stopping"
		}
		return "cancelled"
	}

	release, err := s.hostGate.acquire(ctx, [2]int64{t.OwnerID, t.HostID})
	if err != nil {
		if errors.Is(err, errGateBusy) {
			keep("cancelled:busy")
		} else {
			keep(gone())
		}
		return
	}
	defer release()

	// As recorded now: opened again from a browser meanwhile, it is not this one.
	cur, err := s.store.OpenTabByID(ctx, t.OwnerID, t.SessionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		out.outcome = "gone"
		return
	case err != nil:
		keep(dbOr(ctx, gone))
		return
	}
	t, out.t = cur, cur
	// (Names as an open takes them: the command quotes them, but a record
	// is read back, not asked for.)
	if !tmuxNameRE.MatchString(t.Name) || (t.Kind != store.PersistTmux && t.Kind != store.PersistHerdr) {
		drop("skipped:bad_record")
		return
	}
	now := time.Now()
	if now.Sub(time.Unix(t.AliveAt, 0)) > restoreFresh {
		drop("skipped:stale")
		return
	}
	// (Since when nobody looked; not known, since it was last known to run.)
	idle := time.Unix(t.IdleSince, 0)
	if t.IdleSince == 0 {
		idle = time.Unix(t.AliveAt, 0)
	}
	if now.Sub(idle) > s.cfg.DetachTimeout {
		drop("skipped:idle")
		return
	}
	for _, rs := range s.terms.Running(t.OwnerID, t.HostID) {
		if rs.ID == t.SessionID {
			out.outcome = "gone" // (this gateway's own: recorded after the records were read)
			return
		}
		if rs.HostConfig == t.HostConfig && rs.Persistence == t.Kind && sameMux(sessionKind(t.Kind), rs.Info().MuxSession, t.Name) {
			// (Its record took this one's place as it was opened: not reached.)
			if _, err := s.store.OpenTabByID(ctx, t.OwnerID, rs.ID); err == nil {
				drop("reused")
			} else {
				keep("skipped:inconsistent")
			}
			return
		}
	}

	u, err := s.store.UserByID(ctx, t.OwnerID)
	if err != nil {
		keep(dbOr(ctx, gone))
		return
	}
	out.user = u
	if u.Disabled || u.MustChangePassword {
		drop("skipped:user_disabled")
		return
	}
	switch ok, err := s.auth.HasLogin(ctx, t.OwnerID); {
	case err != nil:
		keep(dbOr(ctx, gone))
		return
	case !ok:
		drop("skipped:no_login")
		return
	}
	h, err := s.store.Host(ctx, t.OwnerID, t.HostID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		drop("skipped:host_gone")
		return
	case err != nil:
		keep(dbOr(ctx, gone))
		return
	}
	out.host = h
	if hostConfig(h) != t.HostConfig {
		drop("skipped:host_changed")
		return
	}
	if h.Auth == store.AuthAsk || h.CredentialID == nil {
		drop("skipped:no_credential")
		return
	}

	// Connecting counts among the user's opens.
	for !s.opening.acquire(t.OwnerID) {
		select {
		case <-ctx.Done():
			keep("cancelled:busy")
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer s.opening.release(t.OwnerID)
	// (Only what is sure removes the record: no key pinned, the credential
	// or the jump host gone or unusable. The database failing, or the jump
	// host's turn busy, keeps it. review526-3)
	switch _, err := s.store.HostKey(ctx, t.OwnerID, t.HostID); {
	case errors.Is(err, store.ErrNotFound):
		drop("skipped:no_host_key")
		return
	case err != nil:
		keep(dbOr(ctx, gone))
		return
	}
	target, err := s.storedTarget(ctx, h)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			keep(gone())
		case errors.Is(err, errGateBusy):
			keep("cancelled:busy")
		case errors.Is(err, store.ErrNotFound):
			drop("skipped:no_credential")
		case errors.Is(err, errJumpGone), errors.Is(err, errJumpChained), errors.Is(err, errJumpNoCredential), errors.Is(err, errJumpNotTrusted):
			drop("failed:jump")
			out.detail = jumpReason(err)
		default:
			keep("cancelled:db")
		}
		return
	}
	dialed, err := sshx.Dial(ctx, s.guard, *target)
	if err != nil {
		var hk *sshx.HostKeyChangedError
		switch {
		case ctx.Err() != nil:
			keep(gone())
		case errors.As(err, &hk):
			drop("failed:host_key")
		default:
			drop("failed:dial")
			out.detail = failureReason(err)
		}
		return
	}
	client := dialed.Client
	started := false
	defer func() {
		if !started {
			client.Close()
		}
	}()

	// Still there?
	opts := term.StartOptions{Cols: t.Cols, Rows: t.Rows, Persistence: t.Kind, HostConfig: t.HostConfig, AttachOnly: true, IdleSince: idle}
	switch there, err := muxRunning(ctx, client, t); {
	case ctx.Err() != nil:
		keep(gone())
		return
	case err != nil:
		drop("failed:probe")
		return
	case !there:
		drop("skipped:not_running")
		return
	}
	switch t.Kind {
	case store.PersistTmux:
		opts.TmuxSession = t.Name
	case store.PersistHerdr:
		if !t.HerdrDefault {
			opts.HerdrSession = t.Name
		}
	}

	res, err := s.terms.Reserve(t.OwnerID)
	switch {
	case errors.Is(err, term.ErrClosing):
		keep("cancelled:stopping")
		return
	case err != nil:
		keep("cancelled:quota") // (the user's terminals: may be only for now)
		return
	}
	defer res.Release()
	// Nothing runs at the host if the host was changed meanwhile; the record
	// takes the old one's place only if that is still there, its user still
	// signed in somewhere and the host as it was, together.
	// (Only a host gone or changed is errHostChanged; the database failing
	// is errRestoreDB, and keeps the record. review526-2)
	opts.PreExec = func(ctx context.Context) error {
		nh, err := s.store.Host(ctx, t.OwnerID, t.HostID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return errHostChanged
		case err != nil && ctx.Err() == nil:
			return fmt.Errorf("%w: %v", errRestoreDB, err)
		case err != nil:
			return ctx.Err()
		case hostConfig(nh) != t.HostConfig:
			return errHostChanged
		}
		return nil
	}
	committed := false
	opts.Register = func(ctx context.Context, sess *term.Session) error {
		rec := tabOf(sess, t.OwnerID, t.HostID, t.HostConfig)
		rec.Name, rec.HerdrDefault = t.Name, t.HerdrDefault // (as recorded: not told again from the name. review526-5)
		err := s.store.ReplaceTab(ctx, t.SessionID, rec, s.auth.LoginRule(),
			func(nh *store.Host) bool { return hostConfig(nh) == t.HostConfig })
		switch {
		case err == nil:
			committed = true
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNoLogin), errors.Is(err, store.ErrHostChanged), ctx.Err() != nil:
		default:
			err = fmt.Errorf("%w: %v", errRestoreDB, err)
		}
		return err
	}
	sess, err := s.terms.Start(ctx, res, t.OwnerID, t.HostID, h.Name, client, opts)
	if err != nil {
		started = true // (Start closed it)
		switch {
		case committed && errors.Is(err, term.ErrClosing):
			out.outcome = "cancelled:stopping" // (the new record kept, as stopped)
		case committed:
			out.outcome = "failed:timeout" // (ended, its record gone; the old one is not written back)
		case errors.Is(err, term.ErrClosing):
			keep("cancelled:stopping")
		case errors.Is(err, errHostChanged), errors.Is(err, store.ErrHostChanged):
			drop("skipped:host_changed")
		case errors.Is(err, store.ErrNoLogin):
			drop("skipped:no_login")
		case errors.Is(err, store.ErrNotFound):
			out.outcome = "gone"
		case ctx.Err() != nil:
			keep(gone())
		case errors.Is(err, errRestoreDB):
			keep("cancelled:db")
		default:
			drop("failed:start")
		}
		return
	}
	started = true
	out.newID = sess.ID
	// Revoked while it started (the record of the new one is then gone with
	// it, or this sees it): closed, stopping or not. Checked within a bound
	// of its own (this one's may be spent), under parent: a check the stop
	// cancelled tells nothing and leaves it to the stop; any other failure
	// to tell closes it. review526-1
	cctx, ccancel := context.WithTimeout(parent, 5*time.Second)
	ok, err := s.auth.HasLogin(cctx, t.OwnerID)
	nh, herr := s.store.Host(cctx, t.OwnerID, t.HostID)
	ccancel()
	if (cancelled(err) || cancelled(herr)) && s.terms.Closing() {
		out.outcome = "cancelled:stopping"
		return
	}
	if err != nil || !ok || herr != nil || hostConfig(nh) != t.HostConfig {
		sess.Close("access revoked")
		out.outcome = "cancelled:revoked"
		return
	}
	if sess.Persistence == store.PersistHerdr {
		go s.watchHerdr(sess, h)
	}
	out.outcome = "restored"
}

// muxRunning: the record's tmux or herdr session is still at the host.
func muxRunning(ctx context.Context, c *ssh.Client, t *store.OpenTab) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	switch t.Kind {
	case store.PersistTmux:
		installed, list, err := tmuxSessions(pctx, c)
		if err != nil || !installed {
			return false, err
		}
		return slices.ContainsFunc(list, func(m tmuxSession) bool { return m.Name == t.Name }), nil
	case store.PersistHerdr:
		list, err := herdr.Discover(pctx, c)
		if errors.Is(err, herdr.ErrNotInstalled) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(list, func(m herdr.SessionInfo) bool {
			return m.Running && (t.HerdrDefault && m.Default || !t.HerdrDefault && !m.Default && m.Name == t.Name)
		}), nil
	}
	return false, nil
}

// dbOr: a database error, unless ctx ended (then why).
func dbOr(ctx context.Context, gone func() string) string {
	if ctx.Err() != nil {
		return gone()
	}
	return "cancelled:db"
}

type restoreOutcome struct {
	t       *store.OpenTab
	user    *store.User
	host    *store.Host
	outcome string
	newID   string
	detail  string
}

func (s *Server) auditRestore(o *restoreOutcome) {
	if o.outcome == "" || o.outcome == "gone" {
		return // (nothing done: a browser opened it again, or it was revoked)
	}
	ctx, cancel := s.endCtx()
	defer cancel()
	uid, name := o.t.OwnerID, ""
	if o.user != nil {
		name = o.user.Username
	}
	target := fmt.Sprintf("host %d", o.t.HostID)
	if o.host != nil {
		target = hostTarget(o.host)
	}
	detail := map[string]any{"session": o.t.SessionID, "outcome": o.outcome, "kind": o.t.Kind, "name": o.t.Name}
	if o.newID != "" {
		detail["new"] = o.newID
	}
	if o.detail != "" {
		detail["reason"] = o.detail
	}
	if err := s.store.Audit(ctx, &uid, name, "session.restore", target, "", detail); err != nil {
		s.log.Error("audit write failed", "action", "session.restore", "err", err)
	}
}
