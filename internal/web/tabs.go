package web

import (
	"context"
	"errors"
	"time"

	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

// The tmux and herdr sessions a user has open, kept in the database
// (store.OpenTab) so that the gateway opens them again by itself after it
// restarts (see restore.go). A record is written as its session is set up,
// before it is published (term.StartOptions.Register): none of its ends can
// come first. It goes with the session, but where the gateway stops: then
// it is kept, with when, and since when nobody had looked.

// tabHeartbeat: how often running sessions are recorded as running (for a
// gateway that is killed, not stopped).
const tabHeartbeat = 10 * time.Minute

// tabOf is the record of a session just set up.
func tabOf(sess *term.Session, owner, host int64, cfg string) *store.OpenTab {
	kind, name := sess.Persistence, sess.Info().MuxSession
	def := false
	if kind == store.PersistHerdr && (name == "" || name == "default") {
		name, def = "default", true // (herdr's default session: one name, see sameMux)
	}
	snap := sess.Snapshot()
	return &store.OpenTab{SessionID: sess.ID, OwnerID: owner, HostID: host, Kind: kind, Name: name, HerdrDefault: def,
		HostConfig: cfg, Cols: snap.Cols, Rows: snap.Rows, AliveAt: time.Now().Unix()}
}

// registerOpened is the Register of a session a person opens (tmux or
// herdr): kept while the login that opened it is valid, in the same
// transaction (a revocation committed before: refused, nothing published).
// The database failing otherwise: opened all the same, not kept (it is not
// opened again after a restart; the browser still does).
func (s *Server) registerOpened(owner, host int64, cfg, login string) func(context.Context, *term.Session) error {
	return func(ctx context.Context, sess *term.Session) error {
		err := s.store.RegisterTab(ctx, tabOf(sess, owner, host, cfg), login, s.auth.LoginRule())
		switch {
		case errors.Is(err, store.ErrNoLogin):
			return err
		case err != nil && ctx.Err() == nil:
			s.log.Warn("open tabs: a session not kept", "err", err)
		}
		return nil
	}
}

// tabEnded keeps or removes the record of a session that ended: kept (with
// when it was last running, and since when nobody looked: then, if someone
// did) where the gateway stopped it; removed otherwise.
func (s *Server) tabEnded(sess *term.Session) {
	if sess.Persistence != store.PersistTmux && sess.Persistence != store.PersistHerdr {
		return
	}
	ctx, cancel := s.endCtx()
	defer cancel()
	snap := sess.Snapshot()
	var err error
	if snap.Reason == term.ReasonShutdown {
		idle := snap.IdleSince
		if idle.IsZero() {
			idle = snap.EndedAt
		}
		err = s.store.KeepTabs(ctx, []store.TabState{{SessionID: sess.ID, OwnerID: sess.OwnerID, AliveAt: snap.EndedAt.Unix(), IdleSince: idle.Unix(), Cols: snap.Cols, Rows: snap.Rows}})
	} else {
		err = s.store.DeleteTab(ctx, sess.OwnerID, sess.ID)
	}
	if err != nil {
		s.log.Warn("open tabs: a session's end not recorded", "session", sess.ID, "err", err)
	}
}

// endCtx bounds a write made as a session ends: by the stop's deadline
// while the gateway stops, else five seconds.
func (s *Server) endCtx() (context.Context, context.CancelFunc) {
	s.stopMu.Lock()
	stop := s.stopCtx
	s.stopMu.Unlock()
	if stop != nil {
		return context.WithCancel(stop)
	}
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// RunTabHeartbeat records the running sessions as running every
// tabHeartbeat, until ctx ends.
func (s *Server) RunTabHeartbeat(ctx context.Context) {
	t := time.NewTicker(tabHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.keepRunningTabs(ctx)
	}
}

func (s *Server) keepRunningTabs(ctx context.Context) {
	now := time.Now().Unix()
	var states []store.TabState
	for _, sess := range s.terms.RunningAll() {
		if sess.Persistence != store.PersistTmux && sess.Persistence != store.PersistHerdr {
			continue
		}
		snap := sess.Snapshot()
		if snap.State != term.StateRunning {
			continue
		}
		var idle int64
		if !snap.IdleSince.IsZero() {
			idle = snap.IdleSince.Unix()
		}
		states = append(states, store.TabState{SessionID: sess.ID, OwnerID: sess.OwnerID, AliveAt: now, IdleSince: idle, Cols: snap.Cols, Rows: snap.Rows})
	}
	if err := s.store.KeepTabs(ctx, states); err != nil && ctx.Err() == nil {
		s.log.Warn("open tabs: running sessions not recorded", "err", err)
	}
}

// BeginShutdown stops what opens sessions: the manager refuses new ones,
// opens under way are cancelled and new ones refused (only those: other
// requests drain; one cancelled would answer "sign in", which signs the
// page out), and writes made as sessions end are bounded by stop (the
// stop's deadline).
func (s *Server) BeginShutdown(stop context.Context) {
	s.stopMu.Lock()
	s.stopCtx = stop
	s.stopMu.Unlock()
	s.terms.BeginShutdown()
	s.sockMu.Lock()
	s.stopping = true
	for _, m := range s.pending {
		for ls := range m {
			if ls.opens {
				ls.cancel()
			}
		}
	}
	s.sockMu.Unlock()
}
