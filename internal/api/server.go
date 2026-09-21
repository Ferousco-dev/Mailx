package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/webhook"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownGrace     = 10 * time.Second
)

// Config wires the API server to its dependencies. Addr, Auth, and Ready
// must all be set by the caller (cmd/mailx) — none of them have a safe
// implicit default.
type Config struct {
	Addr  string
	DB    *database.DB
	Store *storage.FileStore
	// Auth authenticates every /v1 request — v0.19's real replacement for
	// v0.18's DevTenantID (see authmiddleware.go). Production code passes
	// *auth.Service; tests may pass a fake satisfying the same interface.
	Auth authService
	// DomainResolver performs public TXT lookups for ownership verification.
	// Nil selects the system resolver; tests inject a deterministic fake.
	DomainResolver maildomain.TXTResolver
	Webhooks       *webhook.Service
	// DKIM signs outbound mail and manages DKIM keys. Required: without it
	// the API would accept mail from a domain with an active key and send it
	// unsigned.
	DKIM *dkim.Service
	// Logger and Metrics are optional; nil disables the corresponding
	// observation without changing request handling.
	Logger  *slog.Logger
	Metrics *observability.Metrics
	// Ready reports whether MailX can currently meet POST /v1/emails'
	// durability contract (e.g. a live PostgreSQL ping) — see
	// GET /health/ready's doc for why this must not be a fake check.
	Ready func(context.Context) error
}

func (c Config) validate() error {
	if c.Addr == "" {
		return errors.New("api: Addr is empty")
	}
	if c.DB == nil {
		return errors.New("api: DB is nil")
	}
	if c.Store == nil {
		return errors.New("api: Store is nil")
	}
	if c.Auth == nil {
		return errors.New("api: Auth is nil")
	}
	if c.Webhooks == nil {
		return errors.New("api: Webhooks is nil")
	}
	if c.DKIM == nil {
		return errors.New("api: DKIM is nil")
	}
	if c.Ready == nil {
		return errors.New("api: Ready is nil")
	}
	return nil
}

// Server is MailX's HTTP API. It owns nothing but the *http.Server itself
// — PostgreSQL/Redis/FileStore lifecycles remain cmd/mailx's
// responsibility, since other parts of the process (the worker pool,
// the dispatcher) share them too.
type Server struct {
	httpServer *http.Server
}

func NewServer(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	h := newEmailHandler(cfg.DB, cfg.Store)
	resolver := cfg.DomainResolver
	if resolver == nil {
		resolver = maildomain.NewNetTXTResolver()
	}
	domainService := maildomain.NewService(cfg.DB, resolver)
	readiness := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return cfg.Ready(ctx)
	}
	mux := newMux(h, cfg.Auth, readiness, routeServices{domains: domainService, webhooks: cfg.Webhooks, dkim: cfg.DKIM})
	log := cfg.Logger
	if log == nil {
		log = observability.Discard()
	}
	handler := chain(mux, withRequestIDMiddleware(log, cfg.Metrics), withRecoverMiddleware(log), limitBody)

	return &Server{httpServer: &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}}, nil
}

// Run blocks until ctx is canceled, then drains in-flight requests for up
// to shutdownGrace before forcing close. It always returns nil for a
// normal shutdown, matching internal/worker.Pool.Run's convention.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.httpServer.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api: listen: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}
	return nil
}
