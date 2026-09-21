package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

func webhookTestDB(t testing.TB) *database.DB {
	t.Helper()
	dsn := os.Getenv("MAILX_TEST_DATABASE_URL")
	if dsn == "" {
		user := os.Getenv("USER")
		dsn = fmt.Sprintf("postgres://%s@localhost:5432/mailx_test?sslmode=disable", user)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil || admin.Ping(ctx) != nil {
		if admin != nil {
			admin.Close()
		}
		t.Skip("no PostgreSQL available for webhook integration test")
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	schema := "mailx_webhook_test_" + hex.EncodeToString(raw)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, `DROP SCHEMA "`+schema+`" CASCADE`)
		admin.Close()
	})
	db, err := database.Open(ctx, database.Config{DSN: dsn + "&search_path=" + schema, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDurableWebhookRetryAndAcceptedResponseCrashSurviveRestart(t *testing.T) {
	db := webhookTestDB(t)
	ctx := context.Background()
	tenant, err := db.CreateTenant(ctx, "webhook-runtime")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var eventIDs []string
	requests := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		eventIDs = append(eventIDs, r.Header.Get("MailX-Event-Id"))
		if requests == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	box, err := NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	policy := URLPolicy{AllowHTTP: true, AllowPrivate: true}
	service, err := NewService(db, box, policy)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, tenant.ID, endpoint.URL, []string{EventQueued})
	if err != nil {
		t.Fatal(err)
	}
	messageID := make([]byte, 16)
	_, _ = rand.Read(messageID)
	if _, err := db.InsertMessage(ctx, database.NewMessage{
		ID: hex.EncodeToString(messageID), TenantID: tenant.ID, MailFrom: "sender@example.com",
		Recipients: []database.RecipientInput{{Address: "recipient@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := db.FanOutWebhookEvents(ctx, 10); err != nil || result.Deliveries != 1 {
		t.Fatalf("fan-out: %+v %v", result, err)
	}

	first, err := db.ClaimWebhookDelivery(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clientBeforeRestart := NewClient(policy)
	firstOutcome := clientBeforeRestart.Deliver(ctx, first, created.Secret)
	clientBeforeRestart.CloseIdleConnections()
	if firstOutcome.Status != "retrying" || firstOutcome.NextRetryAt == nil {
		t.Fatalf("first outcome: %+v", firstOutcome)
	}
	if err := db.CompleteWebhookAttempt(ctx, database.WebhookAttemptResult{
		DeliveryID: first.ID, LeaseToken: first.LeaseToken, AttemptNumber: first.AttemptCount,
		Status: firstOutcome.Status, CompletedAt: time.Now(), Duration: firstOutcome.Duration,
		ResponseCode: firstOutcome.ResponseCode, ErrorCategory: firstOutcome.ErrorCategory,
		NextRetryAt: firstOutcome.NextRetryAt,
	}); err != nil {
		t.Fatal(err)
	}

	// A fresh client represents a restarted worker process. The retry obligation
	// and stable logical event ID come entirely from PostgreSQL.
	second, err := db.ClaimWebhookDelivery(ctx, firstOutcome.NextRetryAt.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clientAfterRestart := NewClient(policy)
	defer clientAfterRestart.CloseIdleConnections()
	secondOutcome := clientAfterRestart.Deliver(ctx, second, created.Secret)
	if secondOutcome.Status != "succeeded" {
		t.Fatalf("second outcome: %+v", secondOutcome)
	}
	// Simulate a hard crash after the endpoint accepted the request but before
	// MailX committed success. Lease recovery must redeliver the same event.
	third, err := db.ClaimWebhookDelivery(ctx, firstOutcome.NextRetryAt.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	thirdOutcome := clientAfterRestart.Deliver(ctx, third, created.Secret)
	if thirdOutcome.Status != "succeeded" {
		t.Fatalf("third outcome: %+v", thirdOutcome)
	}
	if err := db.CompleteWebhookAttempt(ctx, database.WebhookAttemptResult{
		DeliveryID: third.ID, LeaseToken: third.LeaseToken, AttemptNumber: third.AttemptCount,
		Status: thirdOutcome.Status, CompletedAt: time.Now(), Duration: thirdOutcome.Duration,
		ResponseCode: thirdOutcome.ResponseCode,
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 3 || len(eventIDs) != 3 || eventIDs[0] == "" || eventIDs[0] != eventIDs[1] || eventIDs[1] != eventIDs[2] {
		t.Fatalf("at-least-once identity changed: requests=%d event_ids=%v", requests, eventIDs)
	}
}

func TestWorkerDecryptFailureAndShutdownPreserveDurableState(t *testing.T) {
	db := webhookTestDB(t)
	ctx := context.Background()
	box, err := NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	insertMessage := func(tenantID string) {
		raw := make([]byte, 16)
		_, _ = rand.Read(raw)
		if _, err := db.InsertMessage(ctx, database.NewMessage{
			ID: hex.EncodeToString(raw), TenantID: tenantID, MailFrom: "sender@example.com",
			Recipients: []database.RecipientInput{{Address: "recipient@example.com"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("corrupt encrypted secret fails closed", func(t *testing.T) {
		tenant, _ := db.CreateTenant(ctx, "corrupt-secret")
		sub, err := db.InsertWebhookSubscription(ctx, database.NewWebhookSubscription{
			TenantID: tenant.ID, URL: "http://127.0.0.1:1", EventTypes: []string{EventQueued},
			SecretCiphertext: []byte("corrupt"), SecretNonce: make([]byte, 12),
		})
		if err != nil {
			t.Fatal(err)
		}
		insertMessage(tenant.ID)
		_, _ = db.FanOutWebhookEvents(ctx, 100)
		claim, err := db.ClaimWebhookDelivery(ctx, time.Now().Add(time.Second), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		pool, _ := NewWorkerPool(db, box, devClient(), WorkerConfig{Workers: 1}, nil)
		pool.safeDeliver(ctx, claim)
		rows, err := db.ListWebhookDeliveries(ctx, tenant.ID, sub.ID, 10)
		if err != nil || len(rows) != 1 || rows[0].Status != "failed" || rows[0].LastErrorCategory == nil || *rows[0].LastErrorCategory != "secret_decrypt" {
			t.Fatalf("corrupt-secret result: %+v %v", rows, err)
		}
	})

	t.Run("shutdown cancels request and leaves durable retry", func(t *testing.T) {
		requestStarted := make(chan struct{})
		var once sync.Once
		endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			once.Do(func() { close(requestStarted) })
			<-time.After(5 * time.Second)
		}))
		defer endpoint.Close()
		tenant, _ := db.CreateTenant(ctx, "shutdown")
		service, _ := NewService(db, box, URLPolicy{AllowHTTP: true, AllowPrivate: true})
		created, err := service.Create(ctx, tenant.ID, endpoint.URL, []string{EventQueued})
		if err != nil {
			t.Fatal(err)
		}
		insertMessage(tenant.ID)
		_, _ = db.FanOutWebhookEvents(ctx, 100)
		pool, err := NewWorkerPool(db, box, devClient(), WorkerConfig{
			Workers: 1, ClaimLease: time.Minute, PollInterval: 10 * time.Millisecond,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- pool.Run(runCtx) }()
		select {
		case <-requestStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not start HTTP request")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not shut down promptly")
		}
		rows, err := db.ListWebhookDeliveries(ctx, tenant.ID, created.Subscription.ID, 10)
		if err != nil || len(rows) != 1 || rows[0].Status != "pending" {
			t.Fatalf("shutdown result: %+v %v", rows, err)
		}
	})
}
