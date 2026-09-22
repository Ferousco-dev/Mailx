package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/suppression"
	"github.com/Ferousco-dev/mailx/internal/transfer"
	"github.com/Ferousco-dev/mailx/internal/worker"
)

func persistFailure(t *testing.T, db *database.DB, tenantID string, res delivery.Result, temporary bool, exhausted bool) (suppressed bool) {
	t.Helper()
	ctx := context.Background()
	msg, err := db.InsertMessage(ctx, database.NewMessage{
		ID: "hb" + time.Now().Format("150405.000000000"), TenantID: tenantID, MailFrom: "<a@example.com>",
		Recipients: []database.RecipientInput{{Address: "<dead@example.com>"}, {Address: "<other@example.com>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res.StartedAt, res.FinishedAt = time.Now().UTC(), time.Now().UTC().Add(time.Millisecond)
	state := &retry.State{}
	if err := state.Record(res, &delivery.Error{Kind: res.Kind, Temporary: temporary, Err: errors.New("x")}); err != nil {
		t.Fatal(err)
	}
	attempt, _ := state.Latest()
	outcome := retry.Outcome{Result: res, Status: retry.StatusFailed}
	if temporary {
		outcome = retry.Outcome{Result: res, Status: retry.StatusRetryable, Schedule: &retry.Schedule{NextRetryAt: time.Now().Add(time.Hour)}}
		if exhausted {
			outcome.Status = retry.StatusExhausted
		}
	}
	if err := (databaseOutcomeStore{db: db}).Persist(ctx, msg.ID, attempt, outcome); err != nil {
		t.Fatal(err)
	}
	got, err := db.SuppressedForTenant(ctx, tenantID, []string{"dead@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return got["dead@example.com"]
}

// Which delivery failures justify automatic suppression, through the REAL adapter and database.
func TestOnlyRecipientMailboxRejectionsAutoSuppress(t *testing.T) {
	type c struct {
		name      string
		res       delivery.Result
		temporary bool
		exhausted bool
		want      bool
	}
	perm := func(stage string, code int, enh string, rcpt string) delivery.Result {
		return delivery.Result{Kind: delivery.KindTransferPermanent, Domain: "example.com", FailureStage: stage, FinalCode: code, EnhancedStatus: enh, Recipient: rcpt}
	}
	for _, tc := range []c{
		{"550 5.1.1 at RCPT (recipient does not exist)", perm("rcpt_to", 550, "5.1.1", "<dead@example.com>"), false, false, true},
		{"553 5.1.6 mailbox moved", perm("rcpt_to", 553, "5.1.6", "<dead@example.com>"), false, false, true},
		{"temporary 451 4.1.1 at RCPT", delivery.Result{Kind: delivery.KindTransferTemporary, FailureStage: "rcpt_to", FinalCode: 451, EnhancedStatus: "4.1.1", Recipient: "<dead@example.com>"}, true, false, false},
		{"temporary failures exhausted their retries", delivery.Result{Kind: delivery.KindTransferTemporary, FailureStage: "rcpt_to", FinalCode: 450, EnhancedStatus: "4.2.1", Recipient: "<dead@example.com>"}, true, true, false},
		{"sender rejected at MAIL FROM (5.1.1 about the SENDER)", perm("mail_from", 550, "5.1.1", ""), false, false, false},
		{"policy rejection 5.7.1 at RCPT", perm("rcpt_to", 550, "5.7.1", "<dead@example.com>"), false, false, false},
		{"authentication policy 5.7.26 at RCPT", perm("rcpt_to", 550, "5.7.26", "<dead@example.com>"), false, false, false},
		{"bare 550 without an enhanced code", perm("rcpt_to", 550, "", "<dead@example.com>"), false, false, false},
		{"mailbox full 5.2.2", perm("rcpt_to", 552, "5.2.2", "<dead@example.com>"), false, false, false},
		{"bad domain 5.1.2", perm("rcpt_to", 550, "5.1.2", "<dead@example.com>"), false, false, false},
		{"content rejected at DATA", perm("data", 554, "5.6.0", ""), false, false, false},
		{"relay authentication failure", perm("auth", 535, "5.7.8", ""), false, false, false},
		{"DNS: domain does not exist", delivery.Result{Kind: delivery.KindDNSNotFound, Domain: "example.com"}, false, false, false},
		{"DNS: null MX", delivery.Result{Kind: delivery.KindDNSNullMX, Domain: "example.com"}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newOutcomeTestDB(t)
			tenant, _ := db.CreateTenant(context.Background(), "hb")
			if got := persistFailure(t, db, tenant.ID, tc.res, tc.temporary, tc.exhausted); got != tc.want {
				t.Fatalf("suppressed=%v want %v", got, tc.want)
			}
			// The OTHER recipient is never touched by one recipient's bounce.
			if o, _ := db.SuppressedForTenant(context.Background(), tenant.ID, []string{"other@example.com"}); o["other@example.com"] {
				t.Fatal("a bounce suppressed a different recipient")
			}
		})
	}
}

type mxResolver struct{}

func (mxResolver) LookupMX(_ context.Context, d string) ([]dns.MX, error) {
	return []dns.MX{{Host: "mx." + d, Preference: 10}}, nil
}

type routeToFake struct {
	inner *transfer.Service
	addr  string
}

func (r routeToFake) Transfer(ctx context.Context, req transfer.Request) (transfer.Result, error) {
	_, port, _ := net.SplitHostPort(r.addr)
	req.Destination = net.JoinHostPort("127.0.0.1", port)
	return r.inner.Transfer(ctx, req)
}

type dbLoader struct {
	mu sync.Mutex
	m  map[string]storage.StoredMessage
}

func (l *dbLoader) Load(id string) (storage.StoredMessage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[id], nil
}

// The whole loop with a real database, the production adapter, the real worker and a fake MX:
// a hard bounce suppresses; later mail to that address makes NO SMTP connection; unsuppressing lets
// NEW messages through while old terminal messages never resurrect.
func TestHardBounceThenSuppressedThenUnsuppressedEndToEnd(t *testing.T) {
	db := newOutcomeTestDB(t)
	ctx := context.Background()
	tenant, _ := db.CreateTenant(ctx, "e2e")
	mx := smtptest.Start(t, smtptest.Options{Capture: true, RcptReplies: map[string]string{"dead@": "550 5.1.1 The email account does not exist"}})
	client, err := smtp.NewClient(smtp.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := transfer.NewService(client)
	engine, err := delivery.NewEngine(mxResolver{}, routeToFake{svc, mx.Addr("127.0.0.1")}, delivery.Config{SMTPPort: 25, Shuffle: func([]dns.MX) {}})
	if err != nil {
		t.Fatal(err)
	}
	coord, _ := retry.NewCoordinator(engine, retry.DefaultBackoffPolicy(), retry.DefaultAttemptLimit())
	q, _ := queue.NewMemoryQueue(32)
	loader := &dbLoader{m: map[string]storage.StoredMessage{}}
	pool, err := worker.NewPool(q, loader, coord, databaseOutcomeStore{db: db}, worker.Config{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = pool.Run(pctx); close(done) }()
	defer func() { cancel(); <-done }()

	send := func(id string, rcpts ...string) {
		var in []database.RecipientInput
		for _, r := range rcpts {
			in = append(in, database.RecipientInput{Address: r})
		}
		if _, err := db.InsertMessage(ctx, database.NewMessage{ID: id, TenantID: tenant.ID, MailFrom: "<a@example.com>", Recipients: in}); err != nil {
			t.Fatal(err)
		}
		loader.mu.Lock()
		loader.m[id] = storage.StoredMessage{Raw: []byte("Subject: t\r\n\r\nbody\r\n"), Metadata: storage.StoredMessageMetadata{ID: id, Envelope: storage.StoredEnvelope{MailFrom: "<a@example.com>", RcptTo: rcpts}}}
		loader.mu.Unlock()
		if err := q.Enqueue(ctx, queue.Job{ID: "job-" + id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}
	waitStatus := func(id string, want database.MessageStatus) database.DeliveryState {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := db.LoadDeliveryState(ctx, id); err == nil && st.Status == want {
				return st
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("message %s never reached %s", id, want)
		return database.DeliveryState{}
	}

	// 1. The first message to a dead mailbox bounces permanently at RCPT TO: honest failure history AND a suppression.
	send("m1", "<dead@example.com>")
	st1 := waitStatus("m1", database.StatusFailed)
	if len(st1.Attempts) != 1 || st1.Attempts[0].Accepted {
		t.Fatalf("%+v", st1)
	}
	if got, _ := db.SuppressedForTenant(ctx, tenant.ID, []string{"dead@example.com"}); !got["dead@example.com"] {
		t.Fatal("the recipient hard bounce did not create a suppression")
	}
	rows, _ := db.ListSuppressions(ctx, tenant.ID, 5, nil, "")
	if len(rows) != 1 || rows[0].Reason != suppression.ReasonHardBounce || rows[0].Source != suppression.SourceDelivery || *rows[0].EnhancedStatus != "5.1.1" || *rows[0].MessageID != "m1" {
		t.Fatalf("%+v", rows)
	}
	conns := mx.Conns.Load()

	// 2. A later message to the suppressed address makes ZERO SMTP connections and ends 'suppressed', not failed.
	send("m2", "<Dead@Example.COM>")
	st2 := waitStatus("m2", database.StatusSuppressed)
	if mx.Conns.Load() != conns || len(st2.Attempts) != 0 {
		t.Fatalf("SMTP was attempted for a suppressed recipient: conns %d->%d attempts=%d", conns, mx.Conns.Load(), len(st2.Attempts))
	}

	// 3. Mixed message: the healthy recipient is delivered, the suppressed one is skipped (recipient states truthful).
	send("m3", "<alive@example.com>", "<dead@example.com>")
	st3 := waitStatus("m3", database.StatusDelivered)
	if len(st3.Attempts) != 1 || !st3.Attempts[0].Accepted {
		t.Fatalf("%+v", st3)
	}
	recs, _ := db.ListRecipients(ctx, "m3")
	got := map[string]database.RecipientStatus{}
	for _, r := range recs {
		got[r.Address] = r.Status
	}
	if got["<dead@example.com>"] != database.RecipientSuppressed || got["<alive@example.com>"] != database.RecipientPending {
		t.Fatalf("%v", got)
	}
	if rc := strings.Join(mx.Commands(), " "); !strings.Contains(strings.ToLower(rc), "alive@example.com") {
		t.Fatalf("the healthy recipient must have been sent: %s", rc)
	}

	// 4. Unsuppress: NEW mail may be attempted again (and bounces again, re-suppressing), while
	// the earlier terminal messages are untouched.
	if err := db.DeleteSuppression(ctx, tenant.ID, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := db.LoadDeliveryState(ctx, "m2"); s.Status != database.StatusSuppressed {
		t.Fatalf("unsuppressing resurrected an old message: %+v", s)
	}
	if s, _ := db.LoadDeliveryState(ctx, "m1"); s.Status != database.StatusFailed {
		t.Fatalf("%+v", s)
	}
	before := mx.Conns.Load()
	send("m4", "<dead@example.com>")
	waitStatus("m4", database.StatusFailed)
	if mx.Conns.Load() <= before {
		t.Fatal("after unsuppression the new message must be attempted")
	}
	time.Sleep(200 * time.Millisecond) // nothing old was re-queued or re-sent
	if s, _ := db.LoadDeliveryState(ctx, "m2"); s.Status != database.StatusSuppressed || len(s.Attempts) != 0 {
		t.Fatalf("%+v", s)
	}
}
