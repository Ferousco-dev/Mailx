package database

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/feedback"
)

func permFB(recipient string, raw byte) feedback.Feedback {
	return feedback.Feedback{
		Kind: feedback.KindBouncePermanent, Source: feedback.SourceDSN, Action: feedback.ActionFailed,
		EnhancedStatus: "5.1.1", FinalRecipient: recipient, ReceivedAt: time.Now().UTC(),
		RawSHA256: sha256.Sum256([]byte{raw}),
	}
}

func TestProcessFeedbackPermanentBounceSuppressesAndEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.ProcessFeedback(ctx, msg.ID, permFB("bob@example.com", 1))
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	sup, err := db.SuppressedForTenant(ctx, tn.ID, []string{"bob@example.com"})
	if err != nil || !sup["bob@example.com"] {
		t.Fatalf("not suppressed: %v %v", sup, err)
	}
	evs, err := db.ListTenantPublicEvents(ctx, tn.ID, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Type == EventBounced {
			found = true
		}
	}
	if !found {
		t.Fatal("no bounced event")
	}
}

func TestProcessFeedbackDuplicateBytesIsNoOp(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	fb := permFB("bob@example.com", 5)
	if _, err := db.ProcessFeedback(ctx, msg.ID, fb); err != nil {
		t.Fatal(err)
	}
	created, err := db.ProcessFeedback(ctx, msg.ID, fb)
	if err != nil || created {
		t.Fatalf("duplicate: created=%v err=%v", created, err)
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='bounced'`, msg.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d bounced events, want 1", n)
	}
}

func TestProcessFeedbackDifferentBytesSameTransitionDoesNotReSuppressOrReEvent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if _, err := db.ProcessFeedback(ctx, msg.ID, permFB("bob@example.com", 1)); err != nil {
		t.Fatal(err)
	}
	// A second, byte-different DSN saying the same thing again: new history row,
	// but no second suppression write and no second event.
	if _, err := db.ProcessFeedback(ctx, msg.ID, permFB("bob@example.com", 2)); err != nil {
		t.Fatal(err)
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='bounced'`, msg.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d bounced events, want exactly 1", n)
	}
	db.pool.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE message_id=$1`, msg.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("%d feedback history rows, want 2", n)
	}
}

// A permanent bounce whose enhanced status is NOT a mailbox-invalidity code
// (e.g. 5.7.1, a policy rejection) must never suppress, mirroring
// suppression.QualifiesHardBounce's synchronous rule.
func TestProcessFeedbackPermanentNonMailboxStatusNeverSuppresses(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	fb := feedback.Feedback{Kind: feedback.KindBouncePermanent, Source: feedback.SourceDSN, Action: feedback.ActionFailed,
		EnhancedStatus: "5.7.1", FinalRecipient: "bob@example.com", ReceivedAt: time.Now(), RawSHA256: sha256.Sum256([]byte{11})}
	if _, err := db.ProcessFeedback(ctx, msg.ID, fb); err != nil {
		t.Fatal(err)
	}
	sup, _ := db.SuppressedForTenant(ctx, tn.ID, []string{"bob@example.com"})
	if sup["bob@example.com"] {
		t.Fatal("5.7.1 (policy) must never suppress")
	}
}

func TestProcessFeedbackTemporaryNeverSuppresses(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	fb := feedback.Feedback{Kind: feedback.KindBounceTemporary, Source: feedback.SourceDSN, Action: feedback.ActionDelayed,
		EnhancedStatus: "4.4.7", FinalRecipient: "bob@example.com", ReceivedAt: time.Now(), RawSHA256: sha256.Sum256([]byte{9})}
	if _, err := db.ProcessFeedback(ctx, msg.ID, fb); err != nil {
		t.Fatal(err)
	}
	sup, _ := db.SuppressedForTenant(ctx, tn.ID, []string{"bob@example.com"})
	if sup["bob@example.com"] {
		t.Fatal("temporary feedback must never suppress")
	}
}

func TestProcessFeedbackUnknownNeverSuppresses(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	fb := feedback.Feedback{Kind: feedback.KindBounceUnknown, Source: feedback.SourceDSN,
		FinalRecipient: "bob@example.com", ReceivedAt: time.Now(), RawSHA256: sha256.Sum256([]byte{10})}
	if _, err := db.ProcessFeedback(ctx, msg.ID, fb); err != nil {
		t.Fatal(err)
	}
	sup, _ := db.SuppressedForTenant(ctx, tn.ID, []string{"bob@example.com"})
	if sup["bob@example.com"] {
		t.Fatal("unknown feedback must never suppress")
	}
}

func TestProcessFeedbackComplaintSuppresses(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	fb, err := feedback.FromComplaint("bob@example.com", time.Now(), []byte("complaint-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ProcessFeedback(ctx, msg.ID, fb); err != nil {
		t.Fatal(err)
	}
	sup, _ := db.SuppressedForTenant(ctx, tn.ID, []string{"bob@example.com"})
	if !sup["bob@example.com"] {
		t.Fatal("complaint must suppress")
	}
}

func TestProcessFeedbackUnknownRecipientRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	_, err := db.ProcessFeedback(ctx, msg.ID, permFB("nobody-on-this-message@example.com", 1))
	if err != ErrRecipientNotFound {
		t.Fatalf("%v", err)
	}
}

func TestProcessFeedbackUnknownMessageRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, err := db.ProcessFeedback(ctx, "does-not-exist", permFB("bob@example.com", 1))
	if err != ErrNotFound {
		t.Fatalf("%v", err)
	}
}

// Cross-tenant: feedback correlated to tenant A's message can never suppress
// or affect tenant B, regardless of what the feedback claims.
func TestProcessFeedbackNeverCrossesTenants(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	msgA, _ := db.InsertMessage(ctx, sampleNewMessage(t, a.ID))
	if _, err := db.ProcessFeedback(ctx, msgA.ID, permFB("bob@example.com", 1)); err != nil {
		t.Fatal(err)
	}
	supB, _ := db.SuppressedForTenant(ctx, b.ID, []string{"bob@example.com"})
	if supB["bob@example.com"] {
		t.Fatal("tenant B was affected by tenant A's feedback")
	}
}

func TestProcessFeedbackConcurrentDuplicatesOnlyOneEvent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	msg, _ := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	fb := permFB("bob@example.com", 42)
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() { _, err := db.ProcessFeedback(ctx, msg.ID, fb); errs <- err }()
	}
	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var n int
	db.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='bounced'`, msg.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d bounced events under concurrency, want 1", n)
	}
}
