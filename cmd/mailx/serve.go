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
	"github.com/Ferousco-dev/mailx/internal/auth"
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

	q, err := openRedisQueue()
	if err != nil {
		return err
	}
	defer q.Close()

	store, err := storage.NewFileStore(storageRoot())
	if err != nil {
		return err
	}

	disp := dispatch.New(db, q, dispatch.WithOnError(func(err error) { log.Printf("dispatch: %v", err) }))
	pool, err := buildWorkerPool(q, store, db)
	if err != nil {
		return err
	}
	authSvc := auth.NewService(db, apiKeyPepper())
	apiServer, err := api.NewServer(api.Config{
		Addr: httpAddr(), DB: db, Store: store, Auth: authSvc,
		Ready: func(ctx context.Context) error { return db.Ping(ctx) },
	})
	if err != nil {
		return err
	}
	return runComponents(ctx,
		component{"smtp", func(ctx context.Context) error { return runSMTPReceiver(ctx, store) }},
		component{"dispatch", disp.Run},
		component{"worker", pool.Run},
		component{"api", apiServer.Run},
		component{"idempotency-cleanup", func(ctx context.Context) error { return runIdempotencyCleanup(ctx, db) }},
	)
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

func runIdempotencyCleanup(ctx context.Context, db *database.DB) error {
	ticker := time.NewTicker(idempotencyCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := db.DeleteExpiredIdempotencyKeys(ctx, time.Now().UTC(), idempotencyCleanupBatch)
			if err != nil {
				log.Printf("idempotency-cleanup: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("idempotency-cleanup: removed %d expired key(s)", n)
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
	log.Println("MailX: MAILX_API_KEY_PEPPER is not set — API key verifiers are unkeyed SHA-256 (see internal/auth's doc). Fine for local development; set a pepper before handling real credentials.")
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
