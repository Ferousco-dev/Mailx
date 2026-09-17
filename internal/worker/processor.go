package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/bounce"
	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// processOne drives one claimed job through exactly one retry-level
// delivery operation, then Acks or Releases it based on the outcome.
func (p *Pool) processOne(ctx context.Context, c queue.Claim) {
	durable, err := p.outcomes.Load(ctx, c.Job.MessageID)
	if err != nil {
		p.onError(fmt.Errorf("worker: load durable outcome for job %s: %w", c.Job.ID, err))
		p.release(c, p.now())
		return
	}
	if durable.Terminal {
		if p.ack(c) {
			p.states.delete(c.Job.ID)
		}
		return
	}
	if durable.NextRetryAt != nil && p.now().Before(*durable.NextRetryAt) {
		p.release(c, *durable.NextRetryAt)
		return
	}
	state := p.states.getOrSet(c.Job.ID, durable.RetryState)

	loaded, err := p.loader.Load(c.Job.MessageID)
	if err != nil {
		p.onError(fmt.Errorf("worker: load message for job %s: %w", c.Job.ID, err))
		p.release(c, p.now())
		return
	}

	domain, err := recipientDomain(loaded.Metadata.Envelope.RcptTo)
	if err != nil {
		p.onError(fmt.Errorf("worker: job %s: %w", c.Job.ID, err))
		p.release(c, p.now())
		return
	}

	req := delivery.Request{
		Domain: domain,
		Envelope: mail.Envelope{
			MailFrom:   loaded.Metadata.Envelope.MailFrom,
			Recipients: loaded.Metadata.Envelope.RcptTo,
		},
		Raw: string(loaded.Raw),
	}

	outcome, attemptErr := p.coordinator.Attempt(ctx, state, req, p.now())
	if attemptErr != nil {
		p.handleAttemptError(c, attemptErr)
		return
	}
	latest, ok := state.Latest()
	if !ok {
		p.onError(fmt.Errorf("worker: job %s: coordinator returned without recording an attempt", c.Job.ID))
		p.release(c, p.now())
		return
	}
	if !p.persistOutcome(ctx, c, latest, outcome) {
		return
	}

	switch outcome.Status {
	case retry.StatusSucceeded:
		if p.ack(c) {
			p.states.delete(c.Job.ID)
		}

	case retry.StatusFailed, retry.StatusExhausted:
		p.handleTerminalFailure(loaded, state, outcome.Status, c.Job.ID)
		if p.ack(c) {
			p.states.delete(c.Job.ID)
		}

	case retry.StatusRetryable:
		if outcome.Schedule == nil {
			p.onError(fmt.Errorf("worker: job %s: retryable outcome missing schedule", c.Job.ID))
			p.release(c, p.now())
			return
		}
		p.release(c, outcome.Schedule.NextRetryAt)

	default:
		p.onError(fmt.Errorf("worker: job %s: unrecognized lifecycle status %v", c.Job.ID, outcome.Status))
		p.release(c, p.now())
	}
}

// persistOutcome retries only PostgreSQL persistence while this process still
// knows the SMTP result. It never re-enters Coordinator/SMTP. On shutdown the
// claim is deliberately left unfinalized; a later reclaim starts by loading
// durable state. The remaining hard-crash-before-commit ambiguity is inherent
// because SMTP and PostgreSQL cannot share a transaction.
func (p *Pool) persistOutcome(ctx context.Context, c queue.Claim, attempt retry.DeliveryAttempt, outcome retry.Outcome) bool {
	const retryDelay = 250 * time.Millisecond
	persistTimeout := bookkeepingTimeout
	if leased, ok := p.q.(interface{ ClaimLease() time.Duration }); ok {
		if halfLease := leased.ClaimLease() / 2; halfLease > 0 && halfLease < persistTimeout {
			persistTimeout = halfLease
		}
	}
	first := true
	for {
		// An SMTP operation that finished concurrently with shutdown still gets
		// one bounded chance to commit its outcome. Further retries stop with
		// the pool context so graceful shutdown cannot wait forever on a broken
		// database.
		if !first && ctx.Err() != nil {
			return false
		}
		first = false
		// Refresh before every bounded database attempt. Production's claim
		// lease is much longer than bookkeepingTimeout, so a live worker does
		// not lose ownership while PostgreSQL is unavailable.
		renewCtx, renewCancel := context.WithTimeout(context.Background(), persistTimeout)
		renewErr := p.q.Renew(renewCtx, c.Job.ID, c.Token)
		renewCancel()
		if renewErr != nil {
			p.onError(fmt.Errorf("worker: renew claim for job %s during outcome persistence: %w", c.Job.ID, renewErr))
		}
		persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
		err := p.outcomes.Persist(persistCtx, c.Job.MessageID, attempt, outcome)
		persistCancel()
		if err == nil {
			return true
		}
		if errors.Is(err, ErrOutcomeAlreadyTerminal) {
			if p.ack(c) {
				p.states.delete(c.Job.ID)
			}
			return false
		}
		p.onError(fmt.Errorf("worker: persist outcome for job %s: %w", c.Job.ID, err))
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// handleAttemptError covers the non-nil-error paths of Coordinator.Attempt,
// which normal operation never triggers.
func (p *Pool) handleAttemptError(c queue.Claim, err error) {
	switch {
	case errors.Is(err, retry.ErrNotRetryable), errors.Is(err, retry.ErrRetryExhausted):
		// State was already terminal; ack defensively to stop reprocessing.
		p.onError(fmt.Errorf("worker: job %s: reprocessed an already-terminal retry state: %w", c.Job.ID, err))
		if p.ack(c) {
			p.states.delete(c.Job.ID)
		}
	default:
		p.onError(fmt.Errorf("worker: job %s: coordinator attempt error: %w", c.Job.ID, err))
		p.release(c, p.now())
	}
}

// handleTerminalFailure proves the bounce/DSN path is compatible with a
// worker-driven terminal outcome. It builds and serializes a DSN when
// eligible but does not send it — that needs its own delivery path.
func (p *Pool) handleTerminalFailure(loaded storage.StoredMessage, state *retry.State, status retry.LifecycleStatus, jobID string) {
	failure, err := bounce.Classify(state, status)
	if err != nil {
		p.onError(fmt.Errorf("worker: job %s: bounce classify: %w", jobID, err))
		return
	}
	eligible, err := bounce.ShouldGenerate(loaded.Metadata.Envelope.MailFrom, failure)
	if err != nil || !eligible {
		return
	}
	dsn, err := bounce.NewDSN(failure, loaded.Metadata.Envelope.RcptTo, p.reportingMTA, jobID, p.now())
	if err != nil {
		p.onError(fmt.Errorf("worker: job %s: build DSN: %w", jobID, err))
		return
	}
	if _, err := bounce.Generate(dsn, bounce.MessageOptions{
		From: "MailX Mailer Daemon <postmaster@" + p.reportingMTA + ">",
		To:   loaded.Metadata.Envelope.MailFrom,
		Date: p.now(),
	}); err != nil {
		p.onError(fmt.Errorf("worker: job %s: generate DSN: %w", jobID, err))
	}
}

// recipientDomain uses the first recipient's domain; delivery.Engine
// itself rejects mixed-domain envelopes, so that is sufficient here.
func recipientDomain(recipients []string) (string, error) {
	if len(recipients) == 0 {
		return "", errors.New("envelope has no recipients")
	}
	addr := strings.TrimSpace(recipients[0])
	if !strings.HasPrefix(addr, "<") || !strings.HasSuffix(addr, ">") || len(addr) < 2 {
		return "", fmt.Errorf("recipient %q is not addressable", addr)
	}
	inner := addr[1 : len(addr)-1]
	at := strings.LastIndexByte(inner, '@')
	if at < 0 || at == len(inner)-1 {
		return "", fmt.Errorf("recipient %q is not addressable", addr)
	}
	return inner[at+1:], nil
}
