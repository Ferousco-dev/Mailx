// Package broadcast turns a durably-accepted Broadcast into normal MailX
// messages, through bounded, resumable, crash-safe background work. It is
// orchestration ONLY: rendering/MIME/DKIM/persistence are the EXISTING
// primitives (internal/outbound, internal/dkim, internal/mail,
// internal/storage, database.InsertMessage) — this package creates no
// second delivery path. See docs/design-v0.36.md.
package broadcast

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/emailtemplate"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/outbound"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/Ferousco-dev/mailx/internal/suppression"
)

const (
	defaultInterval         = time.Second
	defaultBroadcastBatch   = 5   // broadcasts touched per tick
	defaultSnapshotBatch    = 200 // audience_members rows scanned per broadcast per tick
	defaultMaterializeBatch = 25  // recipients rendered/signed/persisted per broadcast per tick
	// erasedCleanupRetryDelay separates the 3 immediate retries of an
	// erased-recipient's disk cleanup (see materializeOne) - short, since
	// this is meant to ride out a momentary filesystem error within one
	// tick, not to wait for a longer outage (a longer outage needs the
	// Error-level log/metric this path already emits, not a longer sleep).
	erasedCleanupRetryDelay = 50 * time.Millisecond
)

// errDeterministic marks a materializeOne failure as one that will fail the
// SAME way on every retry, given the SAME frozen inputs (template render,
// MIME build, or MailX's own re-parse of what it just built) — as opposed
// to a transient failure (DKIM key lookup, filesystem, database) that may
// succeed on the very next tick. Only errors.Is(err, errDeterministic)
// consume the bounded terminal-failure budget in materializeBatch; a
// transient failure retries indefinitely, exactly like every other
// existing MailX retry path (PR review: counting transient errors here
// could permanently drop a recipient that a later tick would have
// delivered).
var errDeterministic = errors.New("deterministic materialization failure")

// classifyDKIMError splits a dkim.SignMessage failure by dkim.SignError.
// Outcome (PR review, 3rd round): OutcomeKeyUnavailable is the key-store
// lookup itself failing — genuinely transient (a momentary DB issue),
// always retried. Every other SignError outcome (corrupt/undecryptable key
// material, a public key that no longer matches, a domain mismatch, or a
// hard sign failure) reflects the domain's STORED key state, which retrying
// does not change — deterministic, eligible for the bounded terminal-
// failure counter, so a permanently-broken key cannot block its broadcast
// from completing forever.
func classifyDKIMError(err error) error {
	var signErr *dkim.SignError
	if errors.As(err, &signErr) && signErr.Outcome != dkim.OutcomeKeyUnavailable {
		return fmt.Errorf("dkim sign: %w: %w", errDeterministic, err)
	}
	return fmt.Errorf("dkim sign: %w", err)
}

// Limiter is the narrow ratelimit.Store surface the expander needs — the
// SAME tenant recipient bucket a normal /v1/emails send charges (v0.31), so
// a broadcast cannot buy more throughput than any other send path.
type Limiter interface {
	Allow(ctx context.Context, buckets ...ratelimit.Bucket) (ratelimit.Decision, error)
	TenantRecipientKey(tenantID string) string
}

type Option func(*Expander)

func WithInterval(d time.Duration) Option { return func(e *Expander) { e.interval = d } }
func WithLogger(l *slog.Logger) Option {
	return func(e *Expander) {
		if l != nil {
			e.log = l
		}
	}
}
func WithMetrics(m *observability.Metrics) Option { return func(e *Expander) { e.metrics = m } }
func WithOnError(fn func(error)) Option           { return func(e *Expander) { e.onError = fn } }

// Expander polls for broadcasts needing work (mirrors internal/dispatch's
// outbox-polling pattern) and advances each by one bounded step.
type Expander struct {
	db        *database.DB
	store     *storage.FileStore
	dkim      *dkim.Service
	limiter   Limiter
	policy    ratelimit.Policy
	msgDomain string
	now       func() time.Time
	interval  time.Duration
	log       *slog.Logger
	metrics   *observability.Metrics
	onError   func(error)
}

func New(db *database.DB, store *storage.FileStore, dkimSvc *dkim.Service, limiter Limiter, policy ratelimit.Policy, msgDomain string, opts ...Option) *Expander {
	e := &Expander{
		db: db, store: store, dkim: dkimSvc, limiter: limiter, policy: policy, msgDomain: msgDomain,
		now: func() time.Time { return time.Now().UTC() }, interval: defaultInterval,
		log: observability.Discard(), onError: func(error) {},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Run polls until ctx is canceled; always returns nil (shutdown via context
// cancellation is the expected path).
func (e *Expander) Run(ctx context.Context) error {
	for {
		e.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(e.interval):
		}
	}
}

func (e *Expander) tick(ctx context.Context) {
	broadcasts, err := e.db.ClaimActiveBroadcasts(ctx, defaultBroadcastBatch)
	if err != nil {
		e.onError(fmt.Errorf("broadcast: claim active: %w", err))
		return
	}
	for _, b := range broadcasts {
		e.process(ctx, b)
	}
}

// process advances ONE broadcast by one bounded step: at most
// defaultSnapshotBatch audience rows snapshotted and at most
// defaultMaterializeBatch recipients materialized. Never the whole
// audience — see docs/design-v0.36.md "no giant transaction".
func (e *Expander) process(ctx context.Context, b database.Broadcast) {
	if err := e.db.MarkBroadcastExpanding(ctx, b.ID); err != nil {
		e.onError(fmt.Errorf("broadcast %s: mark expanding: %w", b.ID, err))
		return
	}

	if !b.SnapshotComplete {
		n, _, err := e.db.SnapshotBroadcastBatch(ctx, b, defaultSnapshotBatch)
		if err != nil {
			e.onError(fmt.Errorf("broadcast %s: snapshot batch: %w", b.ID, err))
			return
		}
		e.metrics.BroadcastExpansionBatch("snapshot", "ok")
		e.log.Info("broadcast_snapshot_batch", "broadcast_id", b.ID, "advanced", n)
	}

	pending, err := e.db.ClaimPendingBroadcastRecipients(ctx, b.ID, defaultMaterializeBatch)
	if err != nil {
		e.onError(fmt.Errorf("broadcast %s: list pending: %w", b.ID, err))
		return
	}
	if len(pending) > 0 {
		fromDomain, derr := maildomain.FromDomain(b.FromAddress)
		if derr == nil {
			_, derr = e.db.VerifiedSenderDomain(ctx, b.TenantID, fromDomain)
		}
		if derr != nil {
			// Affects EVERY recipient identically: fail the whole broadcast
			// rather than the same per-recipient error thousands of times.
			if ferr := e.db.MarkBroadcastFailed(ctx, b.ID, "from_domain_not_authorized"); ferr != nil {
				e.onError(fmt.Errorf("broadcast %s: mark failed: %w", b.ID, ferr))
			}
			e.metrics.BroadcastExpansionBatch("materialize", "domain_unauthorized")
			return
		}
		e.materializeBatch(ctx, b, fromDomain, pending)
	}

	completed, err := e.db.TryCompleteBroadcast(ctx, b.ID)
	if err != nil {
		e.onError(fmt.Errorf("broadcast %s: try complete: %w", b.ID, err))
		return
	}
	if completed {
		e.log.Info("broadcast_completed", "broadcast_id", b.ID)
		e.metrics.BroadcastExpansionBatch("complete", "ok")
	}
}

// materializeBatch handles ONE batch of already-snapshotted recipients:
// suppression check (the LATE boundary — checked here, not at Broadcast
// acceptance, so a suppression created mid-broadcast still stops
// not-yet-materialized recipients), abuse-control recipient charge, render,
// MIME, DKIM, persist. A refusal (rate limit or backpressure) leaves the
// recipient 'pending' for a later tick — this IS the broadcast's
// backpressure: it never bypasses v0.31, it just proceeds more slowly.
func (e *Expander) materializeBatch(ctx context.Context, b database.Broadcast, fromDomain string, pending []database.BroadcastRecipient) {
	keys := make([]string, 0, len(pending))
	keyOf := make(map[string]string, len(pending))
	for _, r := range pending {
		if k, err := suppression.Normalize(r.Email); err == nil {
			keys = append(keys, k)
			keyOf[r.ID] = k
		}
	}
	suppressed, err := e.db.SuppressedForTenant(ctx, b.TenantID, keys)
	if err != nil {
		e.onError(fmt.Errorf("broadcast %s: suppression check: %w", b.ID, err))
		return // unknown suppression state: fail safe, retry next tick, no sends
	}

	for _, r := range pending {
		if suppressed[keyOf[r.ID]] {
			if err := e.db.MarkBroadcastRecipientSuppressed(ctx, r.ID); err != nil {
				e.onError(fmt.Errorf("broadcast recipient %s: mark suppressed: %w", r.ID, err))
			}
			e.metrics.BroadcastExpansionBatch("materialize", "suppressed")
			continue
		}

		// Idempotency: a message with this id already existing means a prior
		// attempt materialized it and crashed before the status update below —
		// skip straight to marking materialized, WITHOUT charging quota again.
		if _, err := e.db.GetMessage(ctx, b.TenantID, r.ID); err == nil {
			e.finishMaterialized(ctx, r)
			continue
		} else if !errors.Is(err, database.ErrNotFound) {
			e.onError(fmt.Errorf("broadcast recipient %s: check existing message: %w", r.ID, err))
			continue
		}

		if !e.admit(ctx, b) {
			e.metrics.BroadcastExpansionBatch("materialize", "backpressure")
			return // stop this batch; unprocessed rows stay 'pending' for the next tick
		}

		if err := e.materializeOne(ctx, b, fromDomain, r); err != nil {
			e.onError(fmt.Errorf("broadcast recipient %s: materialize: %w", r.ID, err))
			e.metrics.BroadcastExpansionBatch("materialize", "error")
			// Bounded retry, but ONLY for errDeterministic failures (render/
			// MIME-build/parse — the same frozen inputs will fail the same
			// way forever): these must not retry (and recharge quota) forever,
			// or silently block the broadcast from ever completing (PR
			// review). A TRANSIENT failure (DKIM lookup, filesystem, DB) is
			// deliberately NOT counted here — it must keep retrying
			// indefinitely rather than ever drop a recipient that would
			// otherwise have succeeded (second-round PR review: counting
			// transient errors here let 5 unlucky ticks permanently lose a
			// recipient that a 6th tick would have delivered).
			if errors.Is(err, errDeterministic) {
				if ferr := e.db.RecordBroadcastRecipientFailure(ctx, r.ID); ferr != nil {
					e.onError(fmt.Errorf("broadcast recipient %s: record failure: %w", r.ID, ferr))
				}
			}
			continue // one recipient's failure never aborts the rest of the batch
		}
		e.finishMaterialized(ctx, r)
	}
}

func (e *Expander) finishMaterialized(ctx context.Context, r database.BroadcastRecipient) {
	if err := e.db.MarkBroadcastRecipientMaterialized(ctx, r.ID, r.ID); err != nil {
		e.onError(fmt.Errorf("broadcast recipient %s: mark materialized: %w", r.ID, err))
		return
	}
	e.metrics.BroadcastExpansionBatch("materialize", "ok")
}

// admit applies the SAME safety authority as POST /v1/emails (v0.31): the
// tenant queue cap, system backlog, and per-recipient rate bucket. false
// means "not now" — the caller must leave the recipient pending, never
// treat it as an error.
func (e *Expander) admit(ctx context.Context, b database.Broadcast) bool {
	queued, err := e.db.CountTenantQueued(ctx, b.TenantID, e.policy.TenantMaxQueuedMessages)
	if err != nil || queued >= e.policy.TenantMaxQueuedMessages {
		return false
	}
	pending, err := e.db.CountPendingOutbox(ctx, e.policy.MaxPendingDispatch)
	if err != nil || pending >= e.policy.MaxPendingDispatch {
		return false
	}
	if e.limiter == nil {
		return true
	}
	dec, err := e.limiter.Allow(ctx, ratelimit.Bucket{
		Key: e.limiter.TenantRecipientKey(b.TenantID), Rate: e.policy.TenantRecipientRate, Burst: e.policy.TenantRecipientBurst, Cost: 1,
	})
	return err == nil && dec.Allowed
}

// materializeOne renders THIS broadcast's frozen template snapshot with this
// recipient's frozen personalization snapshot, builds MIME, signs, and
// persists through the EXACT SAME path POST /v1/emails uses
// (database.InsertMessage — outbox/queue/worker/SMTP are untouched by this
// package). Rendering happens once, here, before DKIM — never again on
// retry (retries re-enter this function only via the idempotency check in
// materializeBatch, which skips straight past this on a second attempt).
func (e *Expander) materializeOne(ctx context.Context, b database.Broadcast, fromDomain string, r database.BroadcastRecipient) error {
	vars := mergeVariables(b.Variables, r)
	rendered, err := emailtemplate.Render(b.SubjectTemplate, b.TextTemplate, b.HTMLTemplate, vars)
	if err != nil {
		// Deterministic: the same frozen template+variables will render the
		// SAME error on every retry — eligible for the terminal failure
		// counter (see errDeterministic's doc).
		return fmt.Errorf("render: %w: %w", errDeterministic, err)
	}
	now := e.now()
	messageID := "<" + r.ID + "@" + e.msgDomain + ">"
	built, err := outbound.Build(outbound.Request{
		From: b.FromAddress, To: []string{r.Email}, ReplyTo: b.ReplyTo,
		Subject: rendered.Subject, Text: rendered.Text, HTML: rendered.HTML,
		MessageID: messageID, Date: now,
	})
	if err != nil {
		// Deterministic: fixed inputs (frozen subject/text/html, fixed
		// recipient address) either always build or never do.
		return fmt.Errorf("build MIME: %w: %w", errDeterministic, err)
	}
	raw := built.Raw
	if e.dkim != nil {
		signed, _, serr := e.dkim.SignMessage(ctx, b.TenantID, fromDomain, []byte(raw))
		if serr != nil {
			return classifyDKIMError(serr)
		}
		raw = string(signed)
	}
	parsed, err := mail.ParseMessage(raw)
	if err != nil {
		// Deterministic: Build produced something MailX's own parser
		// rejects — a bug in the builder for this fixed input, not a
		// condition that resolves itself on retry.
		return fmt.Errorf("parse built message: %w: %w", errDeterministic, err)
	}
	// NOT deterministic beyond this point: filesystem/DB writes can fail
	// transiently (disk pressure, connection blip) and succeed on the very
	// next tick — never wrapped in errDeterministic (PR review).
	//
	// ErrRecordExists specifically means a PRIOR attempt already saved this
	// exact recipient's file (e.g. Save succeeded, then InsertMessage failed
	// or crashed before this function returned) — that is success, not a
	// failure to retry: retrying Save for the same ID fails with
	// ErrRecordExists on every future attempt too, which without this check
	// would keep the recipient pending forever (PR review, second finding on
	// this same fix).
	if err := e.store.Save(storage.MessageRecord{
		ID: r.ID, ReceivedAt: now,
		Envelope: mail.Envelope{MailFrom: built.From, Recipients: built.Envelope},
		Message:  parsed,
	}); err != nil && !errors.Is(err, storage.ErrRecordExists) {
		return fmt.Errorf("save file: %w", err)
	}
	role := "to"
	_, err = e.db.InsertMessage(ctx, database.NewMessage{
		ID: r.ID, TenantID: b.TenantID, MailFrom: built.From, FromHeader: b.FromAddress,
		Subject: rendered.Subject, MessageIDHeader: messageID,
		Recipients:                  []database.RecipientInput{{Address: built.Envelope[0], HeaderKind: &role}},
		SenderDomain:                fromDomain,
		RequireBroadcastRecipientID: r.ID,
	})
	if errors.Is(err, database.ErrRecipientErased) {
		// The recipient was erased (GDPR gdpr-delete) between being claimed
		// above and this insert — the file just saved above is now for a
		// message that will never exist in the DB; clean it up rather than
		// leaving it orphaned on disk. Treat this exactly like ErrConflict
		// below: a terminal, non-retryable outcome for this recipient (the
		// broadcast_recipients row is gone, so nothing will ever call this
		// function again for r.ID - there is no other retry path).
		//
		// A few immediate retries handle the common transient case (a
		// momentary FS error); a failure that survives all of them is
		// logged at Error (not Warn) and counted so an operator can alert
		// on it, since it means raw mail for an erased subject is left on
		// disk with no durable record to reconcile it later - see RSK-043.
		var delErr error
		for attempt := 0; attempt < 3; attempt++ {
			if delErr = e.store.Delete(r.ID); delErr == nil {
				break
			}
			time.Sleep(erasedCleanupRetryDelay)
		}
		if delErr != nil {
			e.log.Error("erased_recipient_disk_cleanup_failed", "recipient_id", r.ID, "error", delErr.Error())
			e.metrics.BroadcastExpansionBatch("erasure_cleanup", "error")
			// Surface the failure instead of masking it as success: the
			// broadcast_recipients row is already gone, so nothing else will
			// ever retry this cleanup, and returning nil here would also
			// make the caller call finishMaterialized on a row that no
			// longer exists (Greptile P1, PR #22). Deliberately NOT wrapped
			// in errDeterministic - the caller's generic error path treats
			// it as transient (log + metric only, no RecordBroadcastRecipientFailure
			// against a row that's already gone).
			return fmt.Errorf("erased recipient disk cleanup: %w", delErr)
		}
		e.metrics.BroadcastExpansionBatch("erasure_cleanup", "ok")
		return nil
	}
	if err != nil && !errors.Is(err, database.ErrConflict) { // ErrConflict here = already inserted by a prior crashed attempt: idempotent success
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// mergeVariables layers global broadcast variables under this recipient's
// OWN snapshot (attributes, then name/email, captured once at snapshot time
// — never re-read from the live contact, see migration 000019's doc), so
// personalization takes precedence and is exactly as stable as the email
// address itself. name/email are applied LAST and unconditionally (PR
// review: applying them only when the key is absent let a global "name" or
// "email" template variable silently override every recipient's own
// identity, defeating personalization for the whole broadcast).
func mergeVariables(global map[string]string, r database.BroadcastRecipient) map[string]string {
	vars := make(map[string]string, len(global)+len(r.Attributes)+2)
	for k, v := range global {
		vars[k] = v
	}
	for k, v := range r.Attributes {
		vars[k] = v
	}
	if r.Name != "" {
		vars["name"] = r.Name
	}
	vars["email"] = r.Email
	return vars
}
