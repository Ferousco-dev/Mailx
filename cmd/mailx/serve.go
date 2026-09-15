package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dispatch"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/transfer"
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

	ctx, stop := notifyShutdown()
	defer stop()

	db, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	tenantID, err := resolveDevTenant(ctx, db)
	if err != nil {
		return err
	}
	log.Printf("MailX: development tenant id = %s (see internal/api's devTenantMiddleware doc; every /v1 request uses this tenant until v0.19)", tenantID)

	q, err := openRedisQueue()
	if err != nil {
		return err
	}
	defer q.Close()

	store, err := storage.NewFileStore(storageRoot())
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	runErrs := make(chan error, 4)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := runSMTPReceiver(ctx, store); err != nil {
			runErrs <- fmt.Errorf("smtp: %w", err)
		}
	}()

	disp := dispatch.New(db, q, dispatch.WithOnError(func(err error) { log.Printf("dispatch: %v", err) }))
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := disp.Run(ctx); err != nil {
			runErrs <- fmt.Errorf("dispatch: %w", err)
		}
	}()

	pool, err := buildWorkerPool(q, store, db)
	if err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := pool.Run(ctx); err != nil {
			runErrs <- fmt.Errorf("worker: %w", err)
		}
	}()

	apiServer, err := api.NewServer(api.Config{
		Addr: httpAddr(), DB: db, Store: store, DevTenantID: tenantID,
		Ready: func(ctx context.Context) error { return db.Ping(ctx) },
	})
	if err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := apiServer.Run(ctx); err != nil {
			runErrs <- fmt.Errorf("api: %w", err)
		}
	}()
	log.Printf("MailX API listening on %s", httpAddr())

	wg.Wait()
	close(runErrs)
	return errors.Join(collectErrors(runErrs)...)
}

func collectErrors(ch <-chan error) []error {
	var out []error
	for err := range ch {
		out = append(out, err)
	}
	return out
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

// resolveDevTenant is v0.18's temporary tenant mechanism (see
// internal/api/middleware.go's devTenantMiddleware doc in full): with
// MAILX_DEV_TENANT_ID set, that tenant must already exist (fails loudly
// otherwise, rather than silently creating a duplicate); left unset, one
// tenant is created on first startup and its id is logged so a developer
// can pin it — otherwise every restart would mint a fresh tenant and
// orphan the previous one's data behind tenant-scoped queries.
func resolveDevTenant(ctx context.Context, db *database.DB) (string, error) {
	if id := os.Getenv("MAILX_DEV_TENANT_ID"); id != "" {
		if _, err := db.GetTenant(ctx, id); err != nil {
			return "", fmt.Errorf("MAILX_DEV_TENANT_ID=%s: %w", id, err)
		}
		return id, nil
	}
	tenant, err := db.CreateTenant(ctx, "development")
	if err != nil {
		return "", fmt.Errorf("create development tenant: %w", err)
	}
	return tenant.ID, nil
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

func buildWorkerPool(q queue.Queue, store *storage.FileStore, db *database.DB) (*worker.Pool, error) {
	client, err := smtp.NewClient(smtp.ClientConfig{Identity: "mailx.local"})
	if err != nil {
		return nil, err
	}
	svc, err := transfer.NewService(client)
	if err != nil {
		return nil, err
	}
	engine, err := delivery.NewEngine(dns.NewResolver(), svc, delivery.DefaultConfig())
	if err != nil {
		return nil, err
	}
	coordinator, err := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	if err != nil {
		return nil, err
	}

	workers := envInt("MAILX_WORKERS", 4)
	pool, err := worker.NewPool(q, store, coordinator, worker.Config{Workers: workers},
		worker.WithOnError(func(err error) { log.Printf("worker: %v", err) }),
		worker.WithStatusReporter(func(ctx context.Context, messageID string, status retry.LifecycleStatus, deliveredAt time.Time) {
			reportStatus(ctx, db, messageID, status, deliveredAt)
		}),
	)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// reportStatus translates a retry-level outcome into the durable public
// status (see internal/database's StatusRetrying doc) — best-effort: a
// failure to record it is logged, never fatal, since the retry/queue
// state itself (the thing that actually controls redelivery) is already
// durable independent of this.
func reportStatus(ctx context.Context, db *database.DB, messageID string, status retry.LifecycleStatus, deliveredAt time.Time) {
	var dbStatus database.MessageStatus
	var delivered *time.Time
	switch status {
	case retry.StatusSucceeded:
		dbStatus = database.StatusDelivered
		delivered = &deliveredAt
	case retry.StatusFailed, retry.StatusExhausted:
		dbStatus = database.StatusFailed
	case retry.StatusRetryable:
		dbStatus = database.StatusRetrying
	default:
		return
	}
	updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.UpdateMessageStatus(updateCtx, messageID, dbStatus, delivered); err != nil {
		log.Printf("worker: report status for %s: %v", messageID, err)
	}
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
