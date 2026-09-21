package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newOutcomeTestDB(t *testing.T) *database.DB {
	t.Helper()
	dsn := os.Getenv("MAILX_TEST_DATABASE_URL")
	if dsn == "" {
		user := os.Getenv("USER")
		dsn = fmt.Sprintf("postgres://%s@localhost:5432/mailx_test?sslmode=disable", user)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "mailx_outcome_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	db, err := database.Open(ctx, database.Config{DSN: dsn + "&search_path=" + schema})
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = admin.Exec(cleanupCtx, `DROP SCHEMA "`+schema+`" CASCADE`)
		admin.Close()
	})
	return db
}

func TestDatabaseOutcomeStoreRoundTripAccepted(t *testing.T) {
	db := newOutcomeTestDB(t)
	ctx := context.Background()
	tenant, err := db.CreateTenant(ctx, "outcome-adapter")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := db.InsertMessage(ctx, database.NewMessage{
		ID: "adapter-message", TenantID: tenant.ID, MailFrom: "<a@example.com>",
		Recipients: []database.RecipientInput{{Address: "<b@example.com>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	result := delivery.Result{
		DeliveryID: "delivery-1", Kind: delivery.KindAccepted, Accepted: true,
		FinalCode: 250, QuitError: "connection reset after acceptance",
		StartedAt: start, FinishedAt: start.Add(time.Millisecond),
	}
	state := &retry.State{}
	if err := state.Record(result, nil); err != nil {
		t.Fatal(err)
	}
	attempt, _ := state.Latest()
	store := databaseOutcomeStore{db: db}
	if err := store.Persist(ctx, msg.ID, attempt, retry.Outcome{Result: result, Status: retry.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok := loaded.RetryState.Latest()
	if !loaded.Terminal || !ok || !latest.Result.Accepted || latest.Result.QuitError == "" {
		t.Fatalf("adapter lost accepted durable truth: %+v latest=%+v", loaded, latest)
	}
}
