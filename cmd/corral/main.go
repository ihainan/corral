// Command corral runs the Corral web SSH gateway.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/netguard"
	"github.com/ihainan/corral/internal/notify"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
	"github.com/ihainan/corral/internal/vault"
	"github.com/ihainan/corral/internal/web"
	webui "github.com/ihainan/corral/web"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "genkey":
			k := make([]byte, 32)
			if _, err := rand.Read(k); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Println(base64.StdEncoding.EncodeToString(k))
			return
		case "version":
			fmt.Println(version)
			return
		case "serve":
		default:
			fmt.Fprintf(os.Stderr, "usage: corral [serve|genkey|version]\n")
			os.Exit(2)
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("corral stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	v, err := vault.New(cfg.MasterKey)
	if err != nil {
		return err
	}
	authSvc := auth.NewService(st, cfg.LoginIdleTimeout, cfg.LoginAbsoluteLimit)
	terms := term.NewManager(cfg.DetachTimeout, cfg.MaxSessionsPerUser)
	terms.OnEnd = func(s *term.Session, state string, d time.Duration) {
		uid := s.OwnerID
		// (Bounded: the stop waits for these.)
		actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = st.Audit(actx, &uid, "", "session.end", s.HostName,
			"", map[string]any{"session": s.ID, "state": state, "seconds": int(d.Seconds())})
	}
	web.Version = version

	static := webui.FS()
	// For frontend development: serve files straight from a build directory.
	if dir := os.Getenv("CORRAL_STATIC_DIR"); dir != "" {
		static = os.DirFS(dir)
	}

	notifier, err := notify.New(context.Background(), st, log, notify.Options{
		Subscriber:      cfg.Origin(),
		ExtraPushHosts:  splitList(os.Getenv("CORRAL_PUSH_HOSTS")),
		WebhookPrefixes: splitList(os.Getenv("CORRAL_WEBHOOK_PREFIXES")),
		LoginValid:      authSvc.LoginValid,
		WebhookChinese:  strings.HasPrefix(cfg.DefaultLanguage, "zh"),
	})
	if err != nil {
		return fmt.Errorf("notifications: %w", err)
	}

	srv := web.New(web.Deps{
		Config: cfg, Store: st, Auth: authSvc, Vault: v,
		Guard: netguard.New(cfg.AllowedNetworks, cfg.DeniedNetworks),
		Terms: terms, Log: log, Static: static, Notifier: notifier,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if tok, err := authSvc.PrepareSetup(ctx); err != nil {
		return err
	} else if tok != "" {
		log.Info("no accounts yet: open this link to create the first administrator (valid until restart)",
			"url", cfg.Origin()+"/setup#"+tok)
	}

	go pruneLogins(ctx, st, cfg)

	// The sessions kept open over a restart (see internal/web/tabs.go):
	// turned off, the ones kept are forgotten before anyone can connect.
	if os.Getenv("CORRAL_RESTORE_SESSIONS") == "off" {
		if err := st.ClearTabs(ctx); err != nil {
			return fmt.Errorf("forget the sessions kept over the restart: %w", err)
		}
	}
	hbCtx, stopHeartbeat := context.WithCancel(context.Background())
	defer stopHeartbeat()
	hbDone := make(chan struct{})
	go func() {
		srv.RunTabHeartbeat(hbCtx)
		close(hbDone)
	}()

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Bound before serving: a port taken fails the start here.
	addr := cfg.Listen
	if addr == "" {
		addr = ":http"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("corral listening", "addr", cfg.Listen, "public_url", cfg.Origin(), "version", version)
		errc <- hs.Serve(ln)
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// One budget for the whole stop. Nothing opens sessions any more; then
	// requests drain and starts under way finish, together; the heartbeat
	// stops; the terminals close, as stopped (their records kept), and what
	// is written as they end is waited for; then forwards, recordings and
	// background work. The budget spent: left as after a crash.
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.BeginShutdown(stopCtx)
	var drain sync.WaitGroup
	drain.Add(2)
	go func() {
		defer drain.Done()
		hctx, hcancel := context.WithTimeout(stopCtx, 8*time.Second)
		defer hcancel()
		_ = hs.Shutdown(hctx)
	}()
	go func() {
		defer drain.Done()
		terms.WaitStarts(stopCtx)
	}()
	drain.Wait()
	stopHeartbeat()
	select {
	case <-hbDone:
	case <-stopCtx.Done():
	}
	// Terminals first: closing their SSH connections cannot block and unblocks
	// anything still waiting on them (forwards borrowing them included).
	done := make(chan struct{})
	go func() {
		terms.Shutdown()
		if !terms.WaitEnds(stopCtx) {
			log.Warn("gave up waiting for the ends of sessions to be recorded")
		}
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-stopCtx.Done():
		log.Warn("gave up waiting for connections to close")
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func pruneLogins(ctx context.Context, st *store.Store, cfg *config.Config) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		now := time.Now()
		_ = st.PruneLoginSessions(ctx, now.Add(-cfg.LoginIdleTimeout).Unix(), now.Add(-cfg.LoginAbsoluteLimit).Unix())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
