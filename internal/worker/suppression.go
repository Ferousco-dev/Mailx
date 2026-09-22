package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/suppression"
)

// SuppressionGate is the worker's view of suppression policy. A production
// OutcomeStore implements it (compile-time asserted in cmd/mailx), so enforcement
// cannot be silently absent there; test stores that do not implement it simply run
// without suppression.
type SuppressionGate interface {
	// SuppressedRecipients returns which of the canonical keys are suppressed for
	// the tenant owning the message. An error means the policy state is UNKNOWN.
	SuppressedRecipients(ctx context.Context, messageID string, keys []string) (map[string]bool, error)
	// RecordSuppressed durably records the skipped recipients and, when all is
	// true, the terminal 'suppressed' message state, in one transaction. It returns
	// ErrOutcomeAlreadyTerminal when the message is already terminal.
	RecordSuppressed(ctx context.Context, messageID string, suppressed map[string]bool, all bool) error
}

// suppressionDeferral is how long a job waits when suppression state cannot be
// read. It is NOT a delivery attempt: nothing is recorded, retry counters do not
// move, and the message cannot fail because PostgreSQL was briefly unavailable.
const suppressionDeferral = 15 * time.Second

// Bounded results for the suppression check metric.
const (
	checkClear   = "clear"
	checkPartial = "partial"
	checkAll     = "all"
	checkError   = "error"
)

// enforceSuppression runs BEFORE any SMTP work (before the coordinator and any
// transport call, so a suppressed recipient never reaches MAIL FROM/RCPT/DATA).
//
// It returns the recipients that may still be delivered and handled=true when the
// job is finished (every recipient suppressed: the terminal state is persisted and
// only then is the queue job acked, so a crash between the two re-enters this
// path, finds the terminal state and acks again without SMTP) or was deferred
// because suppression state was unknown.
//
// Consistency boundary (truthful, documented): suppression is checked as late as
// is reasonable, once per attempt, immediately before transport. A suppression
// created after this check but before/during the SMTP conversation does not revoke
// that in-flight attempt, and MailX never holds a database lock across network I/O
// to pretend otherwise. Once the remote server accepts DATA, Accepted=true is
// immutable history.
func (p *Pool) enforceSuppression(ctx context.Context, c queue.Claim, recipients []string) (deliverable []string, handled bool) {
	gate, ok := p.outcomes.(SuppressionGate)
	if !ok {
		return recipients, false
	}
	keyOf := make(map[string]string, len(recipients)) // recipient -> canonical key
	var keys []string
	for _, r := range recipients {
		// An address that cannot be keyed can never have been suppressed (the API
		// refuses to accept such recipients since v0.30), so it stays deliverable.
		if k, err := suppression.Normalize(r); err == nil {
			keyOf[r] = k
			keys = append(keys, k)
		}
	}
	suppressed, err := gate.SuppressedRecipients(ctx, c.Job.MessageID, keys)
	if err != nil {
		// FAIL SAFE: unknown policy state must never become "not suppressed, send".
		p.metrics.SuppressionCheck(checkError)
		p.onError(fmt.Errorf("worker: suppression lookup for job %s: %w", c.Job.ID, err))
		p.log.Warn("suppression_lookup_failed", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
		p.release(c, p.now().Add(suppressionDeferral))
		return nil, true
	}
	if len(suppressed) == 0 {
		p.metrics.SuppressionCheck(checkClear)
		return recipients, false
	}
	for _, r := range recipients {
		if k, ok := keyOf[r]; !ok || !suppressed[k] {
			deliverable = append(deliverable, r)
		}
	}
	all := len(deliverable) == 0
	err = gate.RecordSuppressed(ctx, c.Job.MessageID, suppressed, all)
	if errors.Is(err, ErrOutcomeAlreadyTerminal) {
		// Another process already finished this message; never SMTP again.
		if p.ack(c) {
			p.states.delete(c.Job.ID)
		}
		return nil, true
	}
	if err != nil {
		p.metrics.SuppressionCheck(checkError)
		p.onError(fmt.Errorf("worker: record suppression for job %s: %w", c.Job.ID, err))
		p.log.Warn("suppression_record_failed", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
		p.release(c, p.now().Add(suppressionDeferral))
		return nil, true
	}
	if all {
		p.metrics.SuppressionCheck(checkAll)
		p.log.Info("suppressed_terminal", "job_id", c.Job.ID, "message_id", c.Job.MessageID)
		// Durable terminal state is committed; only now is the queue job finalized.
		if p.ack(c) {
			p.states.delete(c.Job.ID)
		}
		return nil, true
	}
	p.metrics.SuppressionCheck(checkPartial)
	return deliverable, false
}
