// Command corral runs the Corral web SSH gateway.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
		_ = st.Audit(context.Background(), &uid, "", "session.end", s.HostName,
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

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("corral listening", "addr", cfg.Listen, "public_url", cfg.Origin(), "version", version)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutCtx)
	// Terminals first: closing their SSH connections cannot block and unblocks
	// anything still waiting on them (forwards borrowing them included). Then
	// the forwarding connections and background work. Bounded, so a stuck
	// host cannot keep the process from exiting.
	done := make(chan struct{})
	go func() {
		terms.Shutdown()
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
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
