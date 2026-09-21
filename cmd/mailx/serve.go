package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/buildinfo"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dispatch"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/transfer"
	"github.com/Ferousco-dev/mailx/internal/webhook"
	"github.com/Ferousco-dev/mailx/internal/worker"
)

// runFull starts the SMTP receiver alongside the v0.18 API/queue/worker
// pipeline when DATABASE_URL is configured, so `docker compose up` and
// plain local development get the whole system in one process. Without
// DATABASE_URL, MailX falls back to the original v0.1-v0.14 SMTP-only
// behavior (serve()) — DB/Redis are opt-in, not silently required.
func runFull() error {
	if os.Getenv("DATABASE_URL") == "" {
		return serve()
	}

	o, err := newObs()
	if err != nil {
		return err
	}
	slog.SetDefault(o.log)
	ctx, stop := notifyShutdown()
	defer stop()
	info := buildinfo.Get()
	o.log.Info("mailx_start", "http_addr", httpAddr(), "smtp_addr", smtpAddr(), "observability_addr", observabilityAddr(),
		"version", info.Version, "commit", info.Commit)
	defer o.log.Info("mailx_stop")

	db, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	q, err := openRedisQueue()
	if err != nil {
		return err
	}
	defer q.Close()

	store, err := storage.NewFileStore(storageRoot())
	if err != nil {
		return err
	}

	ready := readiness(db, q)
	o.metrics.SetQueueDepth(q.Depth)

	disp := dispatch.New(db, q, dispatch.WithOnError(o.errLogger("dispatch")),
		dispatch.WithLogger(o.log), dispatch.WithMetrics(o.metrics))
	pool, err := buildWorkerPool(q, store, db, o)
	if err != nil {
		return err
	}
	authSvc := auth.NewService(db, apiKeyPepper())
	dkimSvc, err := buildDKIM(db, o)
	if err != nil {
		return err
	}
	spfSvc, err := buildSPF(db, o)
	if err != nil {
		return err
	}
	dmarcSvc, err := buildDMARC(db, dkimSvc, spfSvc, o)
	if err != nil {
		return err
	}
	webhookRuntime, err := buildWebhookRuntime(db, o)
	if err != nil {
		return err
	}
	apiServer, err := api.NewServer(api.Config{
		Addr: httpAddr(), DB: db, Store: store, Auth: authSvc,
		Webhooks: webhookRuntime.service, DKIM: dkimSvc, SPF: spfSvc, DMARC: dmarcSvc,
		Ready:  ready.Check,
		Logger: o.log, Metrics: o.metrics,
	})
	if err != nil {
		return err
	}
	components := []component{
		o.logged("smtp", func(ctx context.Context) error { return runSMTPReceiver(ctx, store, o) }),
		o.logged("dispatch", disp.Run),
		o.logged("worker", pool.Run),
		o.logged("webhook-fanout", webhookRuntime.fanout.Run),
		o.logged("webhook-worker", webhookRuntime.workers.Run),
		o.logged("api", apiServer.Run),
		o.logged("idempotency-cleanup", func(ctx context.Context) error { return runIdempotencyCleanup(ctx, db, o) }),
	}
	if addr := observabilityAddr(); addr != "" {
		op := observability.NewServer(addr, observability.OperatorMux(o.metrics, ready))
		components = append(components, o.logged("observability", op.Run))
	}
	return runComponents(ctx, components...)
}

type webhookComponents struct {
	service *webhook.Service
	fanout  *webhook.FanOut
	workers *webhook.WorkerPool
}

func buildWebhookRuntime(db *database.DB, o obs) (webhookComponents, error) {
	key, err := webhook.DecodeMasterKey(os.Getenv("MAILX_WEBHOOK_MASTER_KEY"))
	if err != nil {
		return webhookComponents{}, err
	}
	box, err := webhook.NewSecretBox(key)
	if err != nil {
		return webhookComponents{}, err
	}
	allowInsecure := envBool("MAILX_WEBHOOK_ALLOW_INSECURE")
	policy := webhook.URLPolicy{AllowHTTP: allowInsecure, AllowPrivate: allowInsecure}
	service, err := webhook.NewService(db, box, policy)
	if err != nil {
		return webhookComponents{}, err
	}
	onError := o.errLogger("webhook")
	workers, err := webhook.NewWorkerPool(db, box, webhook.NewClient(policy), webhook.WorkerConfig{
		Workers: envInt("MAILX_WEBHOOK_WORKERS", 4), ClaimLease: 30 * time.Second,
	}, onError)
	if err != nil {
		return webhookComponents{}, err
	}
	service.WithLogger(o.log)
	workers.WithObservability(o.log, o.metrics)
	return webhookComponents{service: service, fanout: webhook.NewFanOut(db, onError), workers: workers}, nil
}

// idempotencyCleanupInterval/Batch are deliberately conservative: this
// deletes only already-expired rows (see database.DeleteExpiredIdempotencyKeys's
// doc — an in-progress row past its own expiry is abandoned, not active
// work), in small bounded batches, so it never competes meaningfully with
// request traffic even on a large table.
const (
	idempotencyCleanupInterval = 10 * time.Minute
	idempotencyCleanupBatch    = 1000
)

func runIdempotencyCleanup(ctx context.Context, db *database.DB, o obs) error {
	ticker := time.NewTicker(idempotencyCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := db.DeleteExpiredIdempotencyKeys(ctx, time.Now().UTC(), idempotencyCleanupBatch)
			if err != nil {
				o.log.Warn("idempotency_cleanup_failed", "error", err.Error())
				continue
			}
			if n > 0 {
				o.log.Info("idempotency_cleanup", "removed", n)
			}
		}
	}
}

type component struct {
	name string
	run  func(context.Context) error
}

// runComponents stops the whole process when one component fails or exits
// unexpectedly. Waiting for all goroutines before observing errors would
// otherwise leave the dispatcher and workers running after a listener fails.
func runComponents(ctx context.Context, components ...component) error {
	if len(components) == 0 {
		return errors.New("mailx: no components configured")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(components))
	var wg sync.WaitGroup
	for _, c := range components {
		wg.Add(1)
		go func(c component) {
			defer wg.Done()
			err := c.run(runCtx)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", c.name, err)
			} else if runCtx.Err() == nil {
				errs <- fmt.Errorf("%s: exited unexpectedly", c.name)
			}
		}(c)
	}

	var all []error
	select {
	case <-ctx.Done():
	case err := <-errs:
		all = append(all, err)
	}
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}

func openDatabase(ctx context.Context) (*database.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	db, err := database.Open(ctx, database.Config{DSN: dsn})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return db, nil
}

// apiKeyPepper loads the optional HMAC pepper for API-key verifier
// hashing (see internal/auth's package doc for exactly what it does and
// does not protect against). Unset is a valid, documented local-dev
// choice — MailX does not refuse to start without one, but never invents
// a default value, since a "default pepper" baked into the binary would
// protect nothing a public source repository can't also read.
func apiKeyPepper() []byte {
	if p := os.Getenv("MAILX_API_KEY_PEPPER"); p != "" {
		return []byte(p)
	}
	slog.Warn("api_key_pepper_not_set", "detail", "API key verifiers are unkeyed SHA-256; fine for local development, set MAILX_API_KEY_PEPPER before handling real credentials")
	return nil
}

func openRedisQueue() (*queue.RedisQueue, error) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	capacity := envInt("MAILX_QUEUE_CAPACITY", 1000)
	lease := envDuration("MAILX_CLAIM_LEASE", 2*time.Minute)
	q, err := queue.NewRedisQueue(queue.RedisConfig{
		Addr: addr, Namespace: "mailx", Capacity: capacity, ClaimLease: lease,
	})
	if err != nil {
		return nil, fmt.Errorf("open redis queue: %w", err)
	}
	return q, nil
}

// engineConfig is the production delivery configuration: direct MX delivery
// unless a trusted relay was explicitly configured.
func engineConfig(relay *delivery.Relay) delivery.Config {
	cfg := delivery.DefaultConfig()
	cfg.Relay = relay
	return cfg
}

func buildWorkerPool(q queue.Queue, store *storage.FileStore, db *database.DB, o obs) (*worker.Pool, error) {
	tlsCfg, err := outboundTLS(o)
	if err != nil {
		return nil, err
	}
	o.log.Info("smtp_tls_configured", "policy", tlsCfg.Policy.String(), "extra_roots", tlsCfg.RootCAs != nil)
	relay, err := outboundRelay()
	if err != nil {
		return nil, err
	}
	transport := "direct"
	if relay != nil {
		transport = "relay"
	}
	o.log.Info("smtp_transport_configured", "transport", transport, "auth", relay != nil && relay.Auth != nil)
	clientCfg := smtp.ClientConfig{Identity: "mailx.local", TLS: tlsCfg}
	if o.metrics != nil {
		clientCfg.AuthObserver = o.metrics
	}
	client, err := smtp.NewClient(clientCfg)
	if err != nil {
		return nil, err
	}
	svc, err := transfer.NewService(client)
	if err != nil {
		return nil, err
	}
	engine, err := delivery.NewEngine(dns.NewResolver(), svc, engineConfig(relay))
	if err != nil {
		return nil, err
	}
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	if err != nil {
		return nil, err
	}

	workers := envInt("MAILX_WORKERS", 4)
	pool, err := worker.NewPool(q, store, coordinator, databaseOutcomeStore{db: db}, worker.Config{Workers: workers},
		worker.WithOnError(o.errLogger("worker")), worker.WithLogger(o.log), worker.WithMetrics(o.metrics),
	)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

func httpAddr() string {
	if a := os.Getenv("MAILX_HTTP_ADDR"); a != "" {
		return a
	}
	return ":8080"
}

func envInt(key string, def int) int {
	if raw := os.Getenv(key); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if raw := os.Getenv(key); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
