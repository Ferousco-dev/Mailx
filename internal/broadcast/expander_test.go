package broadcast

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func newTestExpander(t *testing.T, db *database.DB) *Expander {
	t.Helper()
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// dkim=nil, limiter=nil: signing/rate-limiting disabled for these tests
	// (matches the rest of the codebase's convention for a nil optional
	// dependency); backpressure/quota behavior is covered separately below
	// with a real ratelimit.Store.
	return New(db, store, nil, nil, ratelimit.DefaultPolicy(), "mailx.local",
		WithOnError(func(err error) { t.Logf("expander error: %v", err) }))
}

func setupBroadcastReady(t *testing.T, db *database.DB, n int) (database.Tenant, database.Broadcast) {
	t.Helper()
	ctx := context.Background()
	tn := newTestTenant(t, db)
	verifyTestDomain(t, db, tn.ID, "example.com")
	aud, err := db.CreateAudience(ctx, database.NewAudience{TenantID: tn.ID, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		c, err := db.CreateContact(ctx, database.NewContact{TenantID: tn.ID, Email: fmt.Sprintf("c%d@dest.example", i), Name: fmt.Sprintf("User%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	tmpl, err := db.CreateTemplate(ctx, database.NewTemplate{TenantID: tn.ID, Name: "t", Subject: "Hi {{name}}", Text: "Body for {{name}}"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBroadcast(ctx, database.NewBroadcast{
		TenantID: tn.ID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: "camp",
		FromAddress: "a@example.com", SubjectTemplate: tmpl.Subject, TextTemplate: tmpl.Text,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tn, b
}

func runTicks(e *Expander, ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		e.tick(ctx)
	}
}

func TestExpanderEndToEndMaterializesEveryContact(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	tn, b := setupBroadcastReady(t, db, 5)
	ctx := context.Background()
	runTicks(e, ctx, 10)

	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Status != "completed" {
		t.Fatalf("%+v %v", got, err)
	}
	recipients, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	if err != nil || len(recipients) != 5 {
		t.Fatalf("%v %v", recipients, err)
	}
	for _, r := range recipients {
		if r.Status != "materialized" || r.MessageID == nil {
			t.Fatalf("%+v", r)
		}
		msg, err := db.GetMessage(ctx, tn.ID, *r.MessageID)
		if err != nil || msg.Subject == "" || msg.Subject == "Hi {{name}}" {
			t.Fatalf("rendered subject not persisted: %+v %v", msg, err)
		}
	}
}

// Suppressed contacts must produce ZERO messages and never reach SMTP.
func TestExpanderSuppressedContactZeroMessages(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	tn, b := setupBroadcastReady(t, db, 3)
	ctx := context.Background()
	if _, _, err := db.CreateSuppression(ctx, database.NewSuppression{TenantID: tn.ID, Email: "c1@dest.example", Reason: "manual", Source: "api"}); err != nil {
		t.Fatal(err)
	}
	runTicks(e, ctx, 10)

	recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	found := false
	for _, r := range recipients {
		if r.Email == "c1@dest.example" {
			found = true
			if r.Status != "suppressed" || r.MessageID != nil {
				t.Fatalf("suppressed recipient materialized: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("suppressed contact missing from snapshot")
	}
}

// A suppression created AFTER snapshotting but BEFORE materialization still
// stops that recipient (the late suppression-check boundary).
func TestExpanderLateSuppressionStopsNotYetMaterializedRecipient(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	tn, b := setupBroadcastReady(t, db, 2)
	ctx := context.Background()

	// Snapshot only (not e.tick, which would also materialize a 2-member
	// audience in the same call): drive the DB snapshot step directly.
	db.MarkBroadcastExpanding(ctx, b.ID)
	db.SnapshotBroadcastBatch(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), 200)
	if _, _, err := db.CreateSuppression(ctx, database.NewSuppression{TenantID: tn.ID, Email: "c0@dest.example", Reason: "manual", Source: "api"}); err != nil {
		t.Fatal(err)
	}
	runTicks(e, ctx, 5)

	recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	for _, r := range recipients {
		if r.Email == "c0@dest.example" && r.Status != "suppressed" {
			t.Fatalf("late suppression did not stop the recipient: %+v", r)
		}
	}
}

// Re-processing the SAME already-materialized recipient (simulating "crashed
// after InsertMessage succeeded, before the status update committed") must
// not create a second message.
func TestExpanderMaterializationRetryIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	tn, b := setupBroadcastReady(t, db, 1)
	ctx := context.Background()
	runTicks(e, ctx, 5)

	recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil)
	if len(recipients) != 1 || recipients[0].MessageID == nil {
		t.Fatalf("%+v", recipients)
	}
	msgID := *recipients[0].MessageID
	dbRecipient := database.BroadcastRecipient{ID: recipients[0].ID, Email: recipients[0].Email, Name: "", Attributes: nil}

	// Re-run materialization for the SAME recipient id directly (bypassing
	// PendingBroadcastRecipients, which would no longer return it): this is
	// exactly the idempotency check materializeBatch performs before ever
	// charging quota or calling InsertMessage again.
	e.materializeBatch(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), "example.com", []database.BroadcastRecipient{dbRecipient})

	// Still exactly one message with this id (InsertMessage is a strict
	// PK insert; a second successful insert with the same id is impossible,
	// so this also proves materializeBatch did not error out on the retry).
	if _, err := db.GetMessage(ctx, tn.ID, msgID); err != nil {
		t.Fatalf("message vanished on idempotent retry: %v", err)
	}
}

func mustGetBroadcast(t *testing.T, db *database.DB, tenantID, id string) database.Broadcast {
	t.Helper()
	b, err := db.GetBroadcast(context.Background(), tenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A malformed recipient snapshot (bad email, never producible by the normal
// snapshot path but reachable via a hostile/corrupted row) must not abort
// the rest of its batch — recipient independence.
func TestExpanderOneBadRecipientDoesNotAbortBatch(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	tn, b := setupBroadcastReady(t, db, 2)
	ctx := context.Background()
	db.MarkBroadcastExpanding(ctx, b.ID)
	db.SnapshotBroadcastBatch(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), 200)

	recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	if len(recipients) != 2 {
		t.Fatalf("%d", len(recipients))
	}
	bad := database.BroadcastRecipient{ID: recipients[0].ID, Email: "not-an-email"}
	good := database.BroadcastRecipient{ID: recipients[1].ID, Email: recipients[1].Email}

	e.materializeBatch(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), "example.com", []database.BroadcastRecipient{bad, good})

	goodRow, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	var goodMaterialized, badUntouched bool
	for _, r := range goodRow {
		if r.ID == good.ID && r.Status == "materialized" {
			goodMaterialized = true
		}
		if r.ID == bad.ID && r.Status == "pending" {
			badUntouched = true
		}
	}
	if !goodMaterialized {
		t.Fatal("the good recipient in the same batch was not processed")
	}
	if !badUntouched {
		t.Fatal("the bad recipient should stay pending (retriable), not corrupt state")
	}
}

// Empty audience completes with zero recipients, coherently.
func TestExpanderEmptyAudienceCompletes(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	tn, b := setupBroadcastReady(t, db, 0)
	ctx := context.Background()
	runTicks(e, ctx, 5)
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Status != "completed" {
		t.Fatalf("%+v %v", got, err)
	}
}

// A larger, still-bounded audience must be processed across several ticks,
// never all at once. Proves boundedness without an hours-long test.
func TestExpanderBoundedAcrossManyTicksForLargerAudience(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	db := newTestDB(t)
	e := newTestExpander(t, db)
	const n = 600 // > defaultSnapshotBatch(200) and > defaultMaterializeBatch(25): several ticks required
	tn, b := setupBroadcastReady(t, db, n)
	ctx := context.Background()

	e.tick(ctx)
	after1, _ := db.GetBroadcast(ctx, tn.ID, b.ID)
	if after1.SnapshotComplete {
		t.Fatal("600 members must not snapshot in a single bounded batch (batch size 200)")
	}
	countAfterOneTick := countAllBroadcastRecipients(t, db, tn.ID, b.ID)
	if countAfterOneTick == 0 || countAfterOneTick >= n {
		t.Fatalf("one tick snapshotted %d of %d (expected a bounded partial batch)", countAfterOneTick, n)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		e.tick(ctx)
		got, _ := db.GetBroadcast(ctx, tn.ID, b.ID)
		if got.Status == "completed" {
			if count := countAllBroadcastRecipients(t, db, tn.ID, b.ID); count != n {
				t.Fatalf("%d recipients, want %d", count, n)
			}
			return
		}
	}
	t.Fatal("broadcast did not complete within the deadline")
}

// countAllBroadcastRecipients pages through (the API/DB layer caps a single
// page at 500) to get a total count for test assertions.
func countAllBroadcastRecipients(t *testing.T, db *database.DB, tenantID, broadcastID string) int {
	t.Helper()
	ctx := context.Background()
	total := 0
	var after *database.BroadcastRecipientCursor
	for {
		page, err := db.ListBroadcastRecipients(ctx, tenantID, broadcastID, 500, after)
		if err != nil {
			t.Fatal(err)
		}
		total += len(page)
		if len(page) < 500 {
			return total
		}
		last := page[len(page)-1]
		after = &database.BroadcastRecipientCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// The broadcast's frozen template snapshot must be used for EVERY recipient,
// even ones materialized after the source Template was edited or deleted.
func TestExpanderUsesFrozenTemplateSnapshotNotLiveTemplate(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	verifyTestDomain(t, db, tn.ID, "example.com")
	aud, _ := db.CreateAudience(ctx, database.NewAudience{TenantID: tn.ID, Name: "a"})
	tmpl, _ := db.CreateTemplate(ctx, database.NewTemplate{TenantID: tn.ID, Name: "t", Subject: "Original {{name}}", Text: "x"})
	c, _ := db.CreateContact(ctx, database.NewContact{TenantID: tn.ID, Email: "c0@dest.example", Name: "Alice"})
	db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID)
	b, err := db.CreateBroadcast(ctx, database.NewBroadcast{
		TenantID: tn.ID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: "camp",
		FromAddress: "a@example.com", SubjectTemplate: tmpl.Subject, TextTemplate: tmpl.Text,
	})
	if err != nil {
		t.Fatal(err)
	}

	newSubject := "Edited {{name}}"
	if _, err := db.UpdateTemplate(ctx, tn.ID, tmpl.ID, database.TemplateUpdate{Subject: &newSubject}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteTemplate(ctx, tn.ID, tmpl.ID); err != nil {
		t.Fatal(err)
	}

	runTicks(e, ctx, 5)
	recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil)
	if len(recipients) != 1 || recipients[0].MessageID == nil {
		t.Fatalf("%+v", recipients)
	}
	msg, err := db.GetMessage(ctx, tn.ID, *recipients[0].MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Subject != "Original Alice" {
		t.Fatalf("subject = %q, want the FROZEN snapshot rendered, not the edited/deleted live template", msg.Subject)
	}
}

// TestScheduledBroadcastNotExpandedBeforeSendAt proves the v0.37 no-early-
// activation invariant for Broadcasts: a future send_at must keep the
// broadcast entirely invisible to the SAME expansion poller ClaimActive
// Broadcasts already used pre-v0.37 — no second scheduler.
func TestScheduledBroadcastNotExpandedBeforeSendAt(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	verifyTestDomain(t, db, tn.ID, "example.com")
	aud, err := db.CreateAudience(ctx, database.NewAudience{TenantID: tn.ID, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.CreateContact(ctx, database.NewContact{TenantID: tn.ID, Email: "one@dest.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, database.NewTemplate{TenantID: tn.ID, Name: "t", Subject: "Hi", Text: "Body"})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	b, err := db.CreateBroadcast(ctx, database.NewBroadcast{
		TenantID: tn.ID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: "camp",
		FromAddress: "a@example.com", SubjectTemplate: tmpl.Subject, TextTemplate: tmpl.Text,
		SendAt: &future,
	})
	if err != nil {
		t.Fatal(err)
	}

	runTicks(e, ctx, 5)
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "accepted" {
		t.Fatalf("scheduled broadcast must stay untouched before send_at: %+v", got)
	}
	if recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil); len(recipients) != 0 {
		t.Fatalf("no expansion must have happened yet: %+v", recipients)
	}
}

// TestScheduledBroadcastExpandsOnceDue proves the flip side: once send_at
// has passed, the SAME poller picks the broadcast up through the ordinary
// accepted/expanding criteria — no separate activation step, no second
// scheduler.
func TestScheduledBroadcastExpandsOnceDue(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	verifyTestDomain(t, db, tn.ID, "example.com")
	aud, err := db.CreateAudience(ctx, database.NewAudience{TenantID: tn.ID, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.CreateContact(ctx, database.NewContact{TenantID: tn.ID, Email: "one@dest.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, database.NewTemplate{TenantID: tn.ID, Name: "t", Subject: "Hi", Text: "Body"})
	if err != nil {
		t.Fatal(err)
	}
	// Already due at acceptance (simulates "the scheduled instant has now
	// arrived" without sleeping or faking a clock).
	due := time.Now().UTC().Add(-time.Second)
	b, err := db.CreateBroadcast(ctx, database.NewBroadcast{
		TenantID: tn.ID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: "camp",
		FromAddress: "a@example.com", SubjectTemplate: tmpl.Subject, TextTemplate: tmpl.Text,
		SendAt: &due,
	})
	if err != nil {
		t.Fatal(err)
	}

	runTicks(e, ctx, 5)
	got, err := db.GetBroadcast(ctx, tn.ID, b.ID)
	if err != nil || got.Status != "completed" {
		t.Fatalf("broadcast should complete once due: %+v %v", got, err)
	}
}

// TestScheduledBroadcastSnapshotIsAtAcceptanceNotActivation proves the
// chosen RSK-038 answer: audience_snapshot_at is stamped at ACCEPTANCE
// regardless of send_at, so a contact added to the Audience AFTER
// acceptance (even before the scheduled send_at arrives) is excluded —
// scheduling a Broadcast for later does not widen the RSK-038 window.
func TestScheduledBroadcastSnapshotIsAtAcceptanceNotActivation(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	verifyTestDomain(t, db, tn.ID, "example.com")
	aud, err := db.CreateAudience(ctx, database.NewAudience{TenantID: tn.ID, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	early, err := db.CreateContact(ctx, database.NewContact{TenantID: tn.ID, Email: "early@dest.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, early.ID); err != nil {
		t.Fatal(err)
	}
	tmpl, err := db.CreateTemplate(ctx, database.NewTemplate{TenantID: tn.ID, Name: "t", Subject: "Hi", Text: "Body"})
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(-time.Second)
	b, err := db.CreateBroadcast(ctx, database.NewBroadcast{
		TenantID: tn.ID, AudienceID: aud.ID, TemplateID: tmpl.ID, Name: "camp",
		FromAddress: "a@example.com", SubjectTemplate: tmpl.Subject, TextTemplate: tmpl.Text,
		SendAt: &due,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A member added AFTER acceptance (audience_snapshot_at is stamped at
	// CreateBroadcast time regardless of send_at).
	late, err := db.CreateContact(ctx, database.NewContact{TenantID: tn.ID, Email: "late@dest.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAudienceMember(ctx, tn.ID, aud.ID, late.ID); err != nil {
		t.Fatal(err)
	}

	runTicks(e, ctx, 5)
	recipients, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 10, nil)
	if err != nil || len(recipients) != 1 || recipients[0].ContactID != early.ID {
		t.Fatalf("expected only the pre-acceptance member, got %+v %v", recipients, err)
	}
}

// TestMergeVariablesRecipientIdentityWins is a unit proof of the PR review
// fix: a global "name"/"email" template variable must never override the
// recipient's OWN name/email — previously the guards only applied the
// recipient's fields when the key was absent, so a broadcast-level "name"
// or "email" variable silently gave every recipient the same identity.
func TestMergeVariablesRecipientIdentityWins(t *testing.T) {
	r := database.BroadcastRecipient{Email: "alice@dest.example", Name: "Alice", Attributes: map[string]string{}}
	global := map[string]string{"name": "Global Name", "email": "global@example.com", "offer": "20% off"}
	got := mergeVariables(global, r)
	if got["name"] != "Alice" {
		t.Fatalf("name = %q, want the recipient's own name (Alice), not the global override", got["name"])
	}
	if got["email"] != "alice@dest.example" {
		t.Fatalf("email = %q, want the recipient's own email, not the global override", got["email"])
	}
	if got["offer"] != "20% off" {
		t.Fatalf("unrelated global variable was dropped: %+v", got)
	}
}

// A recipient with no name (r.Name == "") falls back to a global "name" if
// one was supplied — only a NON-EMPTY recipient name should win.
func TestMergeVariablesFallsBackToGlobalNameWhenRecipientHasNone(t *testing.T) {
	r := database.BroadcastRecipient{Email: "bob@dest.example", Attributes: map[string]string{}}
	got := mergeVariables(map[string]string{"name": "Valued Customer"}, r)
	if got["name"] != "Valued Customer" {
		t.Fatalf("name = %q, want the global fallback since the recipient has none", got["name"])
	}
}

// TestExpanderTransientFailureNeverTerminates proves the second-round PR
// review fix: a TRANSIENT materializeOne failure (here, FileStore.Save
// failing because the store's directory was removed out from under it —
// deterministic to reproduce in a test, but NOT an errDeterministic
// condition; a real transient failure looks the same to materializeBatch)
// must retry indefinitely and never consume the terminal-failure budget,
// even past maxRecipientAttempts — only render/MIME-build/parse failures
// may terminate a recipient.
func TestExpanderTransientFailureNeverTerminates(t *testing.T) {
	db := newTestDB(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := New(db, store, nil, nil, ratelimit.DefaultPolicy(), "mailx.local",
		WithOnError(func(err error) { t.Logf("expander error: %v", err) }))
	tn, b := setupBroadcastReady(t, db, 1)
	ctx := context.Background()
	db.MarkBroadcastExpanding(ctx, b.ID)
	db.SnapshotBroadcastBatch(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), 200)

	// Make every FileStore.Save fail from here on (a stand-in for "disk
	// unavailable" — a real transient failure, not a validation error; same
	// technique internal/storage's own TestFileStoreSaveReturnsFilesystemError
	// uses).
	if err := os.Remove(store.MessagesDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.MessagesDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	if len(recipients) != 1 {
		t.Fatalf("%d", len(recipients))
	}
	r := database.BroadcastRecipient{ID: recipients[0].ID, Email: recipients[0].Email}

	for i := 0; i < 8; i++ {
		e.materializeBatch(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), "example.com", []database.BroadcastRecipient{r})
	}

	after, err := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	if err != nil || len(after) != 1 {
		t.Fatalf("%+v %v", after, err)
	}
	if after[0].Status != "pending" {
		t.Fatalf("status = %q after %d transient failures, want still pending — transient failures must never terminate a recipient", after[0].Status, 8)
	}
	if after[0].Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 — transient failures must not consume the terminal-failure budget", after[0].Attempts)
	}
}

// TestClassifyDKIMErrorSplitsByOutcome is a unit proof of the PR review fix:
// only genuinely transient SignError outcomes must stay retriable forever;
// every other outcome reflects broken stored key state and is deterministic.
func TestClassifyDKIMErrorSplitsByOutcome(t *testing.T) {
	transient := &dkim.SignError{Outcome: dkim.OutcomeKeyUnavailable}
	if errors.Is(classifyDKIMError(transient), errDeterministic) {
		t.Fatal("OutcomeKeyUnavailable must stay transient (a momentary key-store lookup failure)")
	}
	for _, outcome := range []string{dkim.OutcomeKeyDecryptFail, dkim.OutcomeKeyInvalid, dkim.OutcomeDomainMismatch, dkim.OutcomeSignFailed} {
		permanent := &dkim.SignError{Outcome: outcome}
		if !errors.Is(classifyDKIMError(permanent), errDeterministic) {
			t.Fatalf("outcome %q must be classified deterministic (retrying does not change stored key state)", outcome)
		}
	}
}

// TestMaterializeOneTreatsExistingFileRecordAsSuccess proves the PR review
// fix for the "partial write" case: if a prior attempt's FileStore.Save
// succeeded but the attempt then crashed/failed before InsertMessage,
// retrying must NOT treat storage.ErrRecordExists as a failure (it would
// recur on every future retry too, permanently blocking the recipient) — it
// must proceed straight to InsertMessage, exactly like a fresh save would.
func TestMaterializeOneTreatsExistingFileRecordAsSuccess(t *testing.T) {
	db := newTestDB(t)
	e := newTestExpander(t, db)
	tn, b := setupBroadcastReady(t, db, 1)
	ctx := context.Background()
	db.MarkBroadcastExpanding(ctx, b.ID)
	db.SnapshotBroadcastBatch(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), 200)

	recipients, _ := db.ListBroadcastRecipients(ctx, tn.ID, b.ID, 100, nil)
	if len(recipients) != 1 {
		t.Fatalf("%d", len(recipients))
	}
	r := database.BroadcastRecipient{ID: recipients[0].ID, Email: recipients[0].Email}

	// First call: a normal materialize, which also writes the file record.
	if err := e.materializeOne(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), "example.com", r); err != nil {
		t.Fatal(err)
	}
	// Second call for the SAME recipient id: the file record already exists
	// (Save would return ErrRecordExists) — this must NOT be treated as a
	// failure; materializeOne must still complete (InsertMessage's own
	// ErrConflict handling makes the DB write idempotent too).
	if err := e.materializeOne(ctx, mustGetBroadcast(t, db, tn.ID, b.ID), "example.com", r); err != nil {
		t.Fatalf("retry after an existing file record must succeed, got: %v", err)
	}
}
