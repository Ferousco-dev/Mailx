package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/suppression"
)

func newSupp(tenantID, email string) NewSuppression {
	return NewSuppression{TenantID: tenantID, Email: email, Reason: suppression.ReasonManual, Source: suppression.SourceAPI}
}

func countSuppressions(t *testing.T, db *DB, tenantID string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(context.Background(), `SELECT count(*) FROM suppressions WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateSuppressionNormalizesAndIsDuplicateSafe(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	first, created, err := db.CreateSuppression(ctx, newSupp(tn.ID, "  <Person@EXAMPLE.Com.>  "))
	if err != nil || !created || first.Email != "person@example.com" || first.Reason != suppression.ReasonManual || first.Source != suppression.SourceAPI {
		t.Fatalf("%+v %v %v", first, created, err)
	}
	// Every spelling of the same address converges on the same row, and the first writer's facts are kept.
	for _, spelling := range []string{"person@example.com", "PERSON@example.com", "<person@example.com>", "Person@Example.Com."} {
		in := newSupp(tn.ID, spelling)
		in.Reason, in.Source = suppression.ReasonHardBounce, suppression.SourceDelivery
		got, created, err := db.CreateSuppression(ctx, in)
		if err != nil || created || got.ID != first.ID || got.Reason != suppression.ReasonManual || got.Source != suppression.SourceAPI {
			t.Fatalf("%q: %+v created=%v %v", spelling, got, created, err)
		}
	}
	if n := countSuppressions(t, db, tn.ID); n != 1 {
		t.Fatalf("%d rows", n)
	}
	// Dots and plus-tags are NOT folded: different keys.
	for _, e := range []string{"john.smith@gmail.com", "johnsmith@gmail.com", "john.smith+news@gmail.com"} {
		if _, created, err := db.CreateSuppression(ctx, newSupp(tn.ID, e)); err != nil || !created {
			t.Fatalf("%s: %v %v", e, created, err)
		}
	}
	// Invalid input is refused before touching the database.
	for _, bad := range []NewSuppression{
		newSupp(tn.ID, "not an email"), newSupp(tn.ID, ""), newSupp(tn.ID, "a@b@c.com"),
		{TenantID: tn.ID, Email: "x@example.com", Reason: "nope", Source: suppression.SourceAPI},
		{TenantID: tn.ID, Email: "x@example.com", Reason: suppression.ReasonManual, Source: "nope"},
		{Email: "x@example.com", Reason: suppression.ReasonManual, Source: suppression.SourceAPI},
	} {
		if _, _, err := db.CreateSuppression(ctx, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// The table's own CHECK is a backstop against an unnormalized key.
	if _, err := db.pool.Exec(ctx, `INSERT INTO suppressions (id, tenant_id, email, reason, source) VALUES ('x', $1, 'Mixed@Case.com', 'manual', 'api')`, tn.ID); err == nil {
		t.Fatal("the schema must refuse a non-canonical key")
	}
}

func TestSuppressionsAreTenantScoped(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	sa, _, _ := db.CreateSuppression(ctx, newSupp(a.ID, "shared@example.com"))
	sb, created, err := db.CreateSuppression(ctx, newSupp(b.ID, "shared@example.com"))
	if err != nil || !created || sa.ID == sb.ID {
		t.Fatalf("the same address in two tenants is two independent entries: %+v %v %v", sb, created, err)
	}
	if _, err := db.GetSuppression(ctx, b.ID, sa.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
	if err := db.DeleteSuppression(ctx, b.ID, sa.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if _, err := db.GetSuppression(ctx, a.ID, sa.ID); err != nil {
		t.Fatalf("the failed cross-tenant delete must not have removed it: %v", err)
	}
	// A suppression in tenant A never applies to tenant B.
	got, err := db.SuppressedForTenant(ctx, b.ID, []string{"only-a@example.com"})
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	_, _, _ = db.CreateSuppression(ctx, newSupp(a.ID, "only-a@example.com"))
	if got, _ = db.SuppressedForTenant(ctx, b.ID, []string{"only-a@example.com"}); len(got) != 0 {
		t.Fatal("tenant A's suppression leaked into tenant B's check")
	}
	if got, _ = db.SuppressedForTenant(ctx, a.ID, []string{"only-a@example.com", "clear@example.com"}); !got["only-a@example.com"] || got["clear@example.com"] {
		t.Fatalf("%v", got)
	}
	// Delete really deletes; and again is not found.
	if err := db.DeleteSuppression(ctx, a.ID, sa.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSuppression(ctx, a.ID, sa.ID); err != ErrNotFound {
		t.Fatalf("%v", err)
	}
}

func TestListSuppressionsKeysetPaginationAndEmailFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn, other := newTestTenant(t, db), newTestTenant(t, db)
	for i := 0; i < 7; i++ {
		if _, _, err := db.CreateSuppression(ctx, newSupp(tn.ID, fmt.Sprintf("user%d@example.com", i))); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _ = db.CreateSuppression(ctx, newSupp(other.ID, "intruder@example.com"))
	var seen []string
	var cursor *SuppressionCursor
	for page := 0; page < 10; page++ {
		rows, err := db.ListSuppressions(ctx, tn.ID, 3+1, cursor, "")
		if err != nil {
			t.Fatal(err)
		}
		n := min(len(rows), 3)
		for _, r := range rows[:n] {
			seen = append(seen, r.Email)
		}
		if len(rows) <= 3 {
			break
		}
		cursor = &SuppressionCursor{CreatedAt: rows[2].CreatedAt, ID: rows[2].ID}
	}
	if len(seen) != 7 || seen[0] != "user6@example.com" || seen[6] != "user0@example.com" {
		t.Fatalf("newest first, complete, no duplicates: %v", seen)
	}
	for _, e := range seen {
		if strings.Contains(e, "intruder") {
			t.Fatal("another tenant's entry leaked into the list")
		}
	}
	rows, err := db.ListSuppressions(ctx, tn.ID, 5, nil, "USER3@Example.com")
	if err != nil || len(rows) != 1 || rows[0].Email != "user3@example.com" {
		t.Fatalf("%v %v", rows, err)
	}
	if _, err := db.ListSuppressions(ctx, tn.ID, 5, nil, "garbage"); err != ErrInvalidSuppression {
		t.Fatalf("%v", err)
	}
	if _, err := db.ListSuppressions(ctx, tn.ID, 0, nil, ""); err == nil {
		t.Fatal("limit 0 accepted")
	}
}

// Concurrency: the UNIQUE constraint, not process memory, decides.
func TestConcurrentSuppressionCreationConvergesOnOneRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	var wg sync.WaitGroup
	var createdCount atomic.Int64
	ids := sync.Map{}
	for i := 0; i < 20; i++ {
		for _, tn := range []Tenant{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				email := []string{"Same@Example.com", "same@example.com", "<SAME@example.com>"}[i%3]
				s, created, err := db.CreateSuppression(ctx, newSupp(tn.ID, email))
				if err != nil {
					t.Errorf("%v", err)
					return
				}
				if created {
					createdCount.Add(1)
				}
				ids.Store(s.ID, tn.ID)
			}()
		}
	}
	wg.Wait()
	if createdCount.Load() != 2 { // exactly one creator per tenant
		t.Fatalf("created %d times, want 2 (one per tenant)", createdCount.Load())
	}
	if countSuppressions(t, db, a.ID) != 1 || countSuppressions(t, db, b.ID) != 1 {
		t.Fatal("duplicate active rows")
	}
	n := 0
	ids.Range(func(_, _ any) bool { n++; return true })
	if n != 2 {
		t.Fatalf("%d distinct ids", n)
	}
}

func TestSuppressionCreateAndDeleteRaceNeverErrorsOrDuplicates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if s, _, err := db.CreateSuppression(ctx, newSupp(tn.ID, "race@example.com")); err != nil && !strings.Contains(err.Error(), "read existing suppression") {
				t.Errorf("create: %v", err)
			} else if err == nil && s.Email != "race@example.com" {
				t.Errorf("%+v", s)
			}
		}()
		go func() {
			defer wg.Done()
			rows, _ := db.ListSuppressions(ctx, tn.ID, 5, nil, "")
			for _, r := range rows {
				if err := db.DeleteSuppression(ctx, tn.ID, r.ID); err != nil && err != ErrNotFound {
					t.Errorf("delete: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if n := countSuppressions(t, db, tn.ID); n > 1 {
		t.Fatalf("%d rows", n)
	}
}

func recipientStatuses(t *testing.T, db *DB, messageID string) map[string]RecipientStatus {
	t.Helper()
	rs, err := db.ListRecipients(context.Background(), messageID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]RecipientStatus{}
	for _, r := range rs {
		out[r.Address] = r.Status
	}
	return out
}

func countEvents(t *testing.T, db *DB, messageID string, typ EventType) int {
	t.Helper()
	evs, err := db.ListMessageEvents(context.Background(), messageID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestSuppressedForMessageUsesTheMessagesTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, a.ID))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = db.CreateSuppression(ctx, newSupp(a.ID, "bob@example.com"))
	_, _, _ = db.CreateSuppression(ctx, newSupp(b.ID, "hidden-bcc@example.com")) // another tenant's: must not apply
	got, err := db.SuppressedForMessage(ctx, msg.ID, []string{"bob@example.com", "hidden-bcc@example.com"})
	if err != nil || !got["bob@example.com"] || got["hidden-bcc@example.com"] {
		t.Fatalf("%v %v", got, err)
	}
	if got, err = db.SuppressedForMessage(ctx, "no-such-message", []string{"bob@example.com"}); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestRecordSuppressedRecipientsPartialThenAllAndReplay(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID)) // recipients <bob@..> and <hidden-bcc@..>
	if err != nil {
		t.Fatal(err)
	}
	// Partial: bob only.
	if err := db.RecordSuppressedRecipients(ctx, msg.ID, map[string]bool{"bob@example.com": true}, false); err != nil {
		t.Fatal(err)
	}
	st := recipientStatuses(t, db, msg.ID)
	if st["<bob@example.com>"] != RecipientSuppressed || st["<hidden-bcc@example.com>"] != RecipientPending {
		t.Fatalf("%v", st)
	}
	state, _ := db.LoadDeliveryState(ctx, msg.ID)
	if state.Terminal() || state.Status == StatusSuppressed {
		t.Fatalf("a partial suppression must not make the message terminal: %+v", state)
	}
	if countEvents(t, db, msg.ID, EventSuppressed) != 1 {
		t.Fatal("one suppressed event expected")
	}
	// Replay (crash recovery) adds nothing.
	if err := db.RecordSuppressedRecipients(ctx, msg.ID, map[string]bool{"bob@example.com": true}, false); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, db, msg.ID, EventSuppressed) != 1 {
		t.Fatal("replay duplicated the event")
	}
	// Now everything is suppressed (e.g. between retries): terminal 'suppressed', not failed/bounced.
	all := map[string]bool{"bob@example.com": true, "hidden-bcc@example.com": true}
	if err := db.RecordSuppressedRecipients(ctx, msg.ID, all, true); err != nil {
		t.Fatal(err)
	}
	state, _ = db.LoadDeliveryState(ctx, msg.ID)
	if state.Status != StatusSuppressed || !state.Terminal() || len(state.Attempts) != 0 {
		t.Fatalf("terminal suppressed with no SMTP attempt recorded: %+v", state)
	}
	if st = recipientStatuses(t, db, msg.ID); st["<hidden-bcc@example.com>"] != RecipientSuppressed {
		t.Fatalf("%v", st)
	}
	if countEvents(t, db, msg.ID, EventSuppressed) != 1 {
		t.Fatal("still exactly one suppressed event per message")
	}
	// A crash-replay of the terminal write is idempotent...
	if err := db.RecordSuppressedRecipients(ctx, msg.ID, all, true); err != nil {
		t.Fatal(err)
	}
	// ...and a later SMTP outcome can never be persisted over it.
	if err := db.PersistDeliveryOutcome(ctx, sampleAttempt(msg.ID, 1, DecisionRetry), false, nil); err != ErrMessageTerminal {
		t.Fatalf("an outcome after terminal suppression must be refused: %v", err)
	}
}

func TestRecordSuppressedNeverRewritesAcceptedDelivery(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	att := sampleAttempt(msg.ID, 1, DecisionTerminalSuccess)
	att.Accepted, att.Kind, att.FinalCode, att.EnhancedStatus, att.FailureStage = true, "accepted", 250, "", ""
	if err := db.PersistDeliveryOutcome(ctx, att, false, nil); err != nil {
		t.Fatal(err)
	}
	// The recipient becomes suppressed AFTER the remote server accepted DATA.
	if err := db.RecordSuppressedRecipients(ctx, msg.ID, map[string]bool{"bob@example.com": true}, true); err != ErrMessageTerminal {
		t.Fatalf("accepted history is immutable: %v", err)
	}
	state, _ := db.LoadDeliveryState(ctx, msg.ID)
	if state.Status != StatusDelivered || !state.Attempts[0].Accepted {
		t.Fatalf("%+v", state)
	}
	if st := recipientStatuses(t, db, msg.ID); st["<bob@example.com>"] != RecipientPending {
		t.Fatalf("recipient rows of a delivered message are untouched: %v", st)
	}
}

// Hard-bounce auto-suppression commits atomically with the outcome.
func TestHardBounceSuppressionIsAtomicWithTheOutcome(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	att := sampleAttempt(msg.ID, 1, DecisionTerminalFailure)
	att.Kind, att.FailureStage, att.Recipient, att.FinalCode, att.EnhancedStatus = "transfer_permanent", "rcpt_to", "<Bob@Example.COM>", 550, "5.1.1"
	att.SuppressRecipient = true
	if err := db.PersistDeliveryOutcome(ctx, att, false, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.ListSuppressions(ctx, tn.ID, 5, nil, "")
	if len(rows) != 1 || rows[0].Email != "bob@example.com" || rows[0].Reason != suppression.ReasonHardBounce || rows[0].Source != suppression.SourceDelivery ||
		rows[0].MessageID == nil || *rows[0].MessageID != msg.ID || rows[0].SMTPCode == nil || *rows[0].SMTPCode != 550 || *rows[0].EnhancedStatus != "5.1.1" {
		t.Fatalf("%+v", rows)
	}
	state, _ := db.LoadDeliveryState(ctx, msg.ID)
	if state.Status != StatusFailed {
		t.Fatalf("history is still an honest failed delivery: %+v", state)
	}
	// Persisting the SAME attempt again (crash replay) is a no-op for suppression too.
	if err := db.PersistDeliveryOutcome(ctx, att, false, nil); err != nil {
		t.Fatal(err)
	}
	if n := countSuppressions(t, db, tn.ID); n != 1 {
		t.Fatalf("%d", n)
	}
	// A refused outcome (message already terminal) creates NO suppression: same transaction.
	msg2, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	done := sampleAttempt(msg2.ID, 1, DecisionTerminalFailure)
	done.Kind, done.FailureStage = "transfer_permanent", "data"
	if err := db.PersistDeliveryOutcome(ctx, done, false, nil); err != nil {
		t.Fatal(err)
	}
	late := sampleAttempt(msg2.ID, 2, DecisionTerminalFailure)
	late.Kind, late.FailureStage, late.Recipient, late.FinalCode, late.EnhancedStatus = "transfer_permanent", "rcpt_to", "<new-victim@example.com>", 550, "5.1.1"
	late.SuppressRecipient = true
	if err := db.PersistDeliveryOutcome(ctx, late, false, nil); err != ErrMessageTerminal {
		t.Fatalf("%v", err)
	}
	if got, _ := db.SuppressedForTenant(ctx, tn.ID, []string{"new-victim@example.com"}); len(got) != 0 {
		t.Fatal("a rolled-back outcome must not leave a suppression behind")
	}
	// SuppressRecipient is ignored for non-terminal-failure decisions (temporary failures never suppress).
	msg3, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	tmp := sampleAttempt(msg3.ID, 1, DecisionRetry)
	tmp.Recipient, tmp.SuppressRecipient = "<temp@example.com>", true
	if err := db.PersistDeliveryOutcome(ctx, tmp, false, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.SuppressedForTenant(ctx, tn.ID, []string{"temp@example.com"}); len(got) != 0 {
		t.Fatal("a temporary failure suppressed a recipient")
	}
}

func TestConcurrentHardBouncesOfOneRecipientCreateOneSuppression(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			att := sampleAttempt(msg.ID, 1, DecisionTerminalFailure)
			att.Kind, att.FailureStage, att.Recipient, att.FinalCode, att.EnhancedStatus = "transfer_permanent", "rcpt_to", "<dead@example.com>", 550, "5.1.1"
			att.SuppressRecipient = true
			if err := db.PersistDeliveryOutcome(ctx, att, false, nil); err != nil {
				t.Errorf("%v", err)
			}
			// ... racing with an API suppression of the same address.
			if _, _, err := db.CreateSuppression(ctx, newSupp(tn.ID, "DEAD@example.com")); err != nil {
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	if n := countSuppressions(t, db, tn.ID); n != 1 {
		t.Fatalf("%d active rows for one address", n)
	}
}

// rollBackPastSuppressions rolls back one migration at a time until the
// suppressions table is gone, so the test never assumes the v0.30 migration is the
// newest one (later migrations sit above it).
func rollBackPastSuppressions(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if err := db.MigrateDownOne(ctx); err != nil {
			t.Fatalf("down migration: %v", err)
		}
		var exists bool
		_ = db.pool.QueryRow(ctx, `SELECT to_regclass('suppressions') IS NOT NULL`).Scan(&exists)
		if !exists {
			return
		}
	}
	t.Fatal("down migrations must drop suppressions")
}

// The v0.30 migration upgrades a v0.29 database in place and its down migration is safe.
func TestSuppressionMigrationUpgradesAndDowngradesExistingData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	// Roll back to the v0.29 schema, create v0.29-era data, then upgrade.
	rollBackPastSuppressions(t, db)
	legacy, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	att := sampleAttempt(legacy.ID, 1, DecisionTerminalFailure)
	att.Kind = "transfer_permanent"
	if err := db.PersistDeliveryOutcome(ctx, att, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade over existing data: %v", err)
	}
	state, err := db.LoadDeliveryState(ctx, legacy.ID)
	if err != nil || state.Status != StatusFailed || len(state.Attempts) != 1 {
		t.Fatalf("existing history must survive the upgrade untouched: %+v %v", state, err)
	}
	if _, _, err := db.CreateSuppression(ctx, newSupp(tn.ID, "after@example.com")); err != nil {
		t.Fatalf("new table usable after upgrade: %v", err)
	}
	// Downgrade with new-vocabulary rows present: mapped, not failed.
	sup, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err := db.RecordSuppressedRecipients(ctx, sup.ID, map[string]bool{"bob@example.com": true, "hidden-bcc@example.com": true}, true); err != nil {
		t.Fatal(err)
	}
	rollBackPastSuppressions(t, db)
	var status string
	_ = db.pool.QueryRow(ctx, `SELECT status FROM messages WHERE id = $1`, sup.ID).Scan(&status)
	if status != "failed" {
		t.Fatalf("suppressed message mapped to %q", status)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-upgrade: %v", err)
	}
	if got, err := db.LoadDeliveryState(ctx, legacy.ID); err != nil || got.Status != StatusFailed {
		t.Fatalf("%+v %v", got, err)
	}
}

// Query plans: the hot lookup and the list query use the intended indexes.
func TestSuppressionQueriesUseIndexesAtScale(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	if _, err := db.pool.Exec(ctx, `INSERT INTO suppressions (id, tenant_id, email, reason, source, created_at)
		SELECT 'sb'||g, $1, 'user'||g||'@example.com', 'manual', 'api', now() - (g || ' seconds')::interval FROM generate_series(1, 60000) g`, tn.ID); err != nil {
		t.Fatal(err)
	}
	other := newTestTenant(t, db)
	if _, err := db.pool.Exec(ctx, `INSERT INTO suppressions (id, tenant_id, email, reason, source)
		SELECT 'so'||g, $1, 'user'||g||'@example.com', 'manual', 'api' FROM generate_series(1, 60000) g`, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE suppressions`); err != nil {
		t.Fatal(err)
	}
	// 1. The delivery hot path: is any of these recipients suppressed? (3 recipients)
	plan := explainAnalyze(t, db, `SELECT email FROM suppressions WHERE tenant_id = $1 AND email = ANY($2)`,
		tn.ID, []string{"user42@example.com", "nobody@example.com", "user59999@example.com"})
	if strings.Contains(plan, "Seq Scan") || !strings.Contains(plan, "uq_suppressions_tenant_email") {
		t.Fatalf("the recipient lookup must use the unique (tenant_id, email) index:\n%s", plan)
	}
	t.Logf("hot lookup plan:\n%s", plan)
	// 2. Through the message (worker path): join messages -> suppressions.
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	plan = explainAnalyze(t, db, `SELECT s.email FROM messages m JOIN suppressions s ON s.tenant_id = m.tenant_id
		WHERE m.id = $1 AND s.email = ANY($2)`, msg.ID, []string{"user42@example.com", "user7@example.com"})
	if strings.Contains(plan, "Seq Scan on suppressions") || !strings.Contains(plan, "uq_suppressions_tenant_email") {
		t.Fatalf("the worker lookup must use the unique index, not scan suppressions:\n%s", plan)
	}
	t.Logf("worker lookup plan:\n%s", plan)
	// 3. Keyset listing: index order, no sort node, bounded by LIMIT.
	plan = explainAnalyze(t, db, `SELECT id FROM suppressions WHERE tenant_id = $1 AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC LIMIT 21`, tn.ID, nil, nil)
	if strings.Contains(plan, "Seq Scan") || strings.Contains(plan, "Sort") || !strings.Contains(plan, "idx_suppressions_tenant_created") {
		t.Fatalf("the list query must use idx_suppressions_tenant_created without sorting:\n%s", plan)
	}
	t.Logf("list plan:\n%s", plan)
}

// The lifecycle event is durable, public, deduplicated, privacy-safe, and reaches webhook subscribers
// through the real fan-out; a webhook problem can never change suppression truth.
func TestSuppressedEventReachesWebhookSubscribersOnceWithoutAddresses(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	sub := insertTestWebhook(t, db, tn.ID, "email.suppressed")
	other := insertTestWebhook(t, db, tn.ID, "email.delivered") // not subscribed to suppression
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if pub, ok := PublicEventType(EventSuppressed); !ok || pub != "email.suppressed" {
		t.Fatalf("%q %v", pub, ok)
	}
	keys := map[string]bool{"bob@example.com": true, "hidden-bcc@example.com": true}
	for i := 0; i < 3; i++ { // replays after crashes must not multiply the event
		if err := db.RecordSuppressedRecipients(ctx, msg.ID, keys, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.FanOutWebhookEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListWebhookDeliveries(ctx, tn.ID, sub.ID, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("subscriber must receive exactly one delivery: %d %v", len(got), err)
	}
	if none, _ := db.ListWebhookDeliveries(ctx, tn.ID, other.ID, 10); len(none) != 0 {
		t.Fatal("a subscriber to other events must not receive it")
	}
	evs, _ := db.ListMessageEvents(ctx, msg.ID)
	for _, e := range evs {
		if e.Type == EventSuppressed {
			b := fmt.Sprint(e.Metadata)
			if strings.Contains(b, "@") || strings.Contains(b, "example.com") {
				t.Fatalf("the event payload carries recipient addresses: %s", b)
			}
		}
	}
	// Webhook delivery trouble is independent of suppression state: the message stays suppressed.
	if _, err := db.pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed' WHERE subscription_id = $1`, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := db.LoadDeliveryState(ctx, msg.ID); st.Status != StatusSuppressed {
		t.Fatalf("%+v", st)
	}
}

// BenchmarkSuppressedForMessage measures the per-attempt worker lookup (message -> tenant ->
// suppression keys) against 120k rows: run with -bench to reproduce the numbers in the design notes.
func BenchmarkSuppressedForMessage(b *testing.B) {
	db := newTestDB(b)
	ctx := context.Background()
	tn := newTestTenant(b, db)
	other := newTestTenant(b, db)
	for _, id := range []string{tn.ID, other.ID} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO suppressions (id, tenant_id, email, reason, source)
			SELECT $1||g, $1, 'user'||g||'@example.com', 'manual', 'api' FROM generate_series(1, 60000) g`, id); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE suppressions`); err != nil {
		b.Fatal(err)
	}
	in := NewMessage{ID: "bench-msg", TenantID: tn.ID, MailFrom: "<a@example.com>", Recipients: []RecipientInput{{Address: "<x@example.com>"}}}
	if _, err := db.InsertMessage(ctx, in); err != nil {
		b.Fatal(err)
	}
	keys := []string{"user42@example.com", "clear-1@example.com", "user59999@example.com"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := db.SuppressedForMessage(ctx, "bench-msg", keys)
		if err != nil || len(got) != 2 {
			b.Fatalf("%v %v", got, err)
		}
	}
}
