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
	loaded, err := p.loader.Load(c.Job.MessageID)
	if err != nil {
		p.onError(fmt.Errorf("worker: load message for job %s: %w", c.Job.ID, err))
		p.releaseBestEffort(c, p.now())
		return
	}

	domain, err := recipientDomain(loaded.Metadata.Envelope.RcptTo)
	if err != nil {
		p.onError(fmt.Errorf("worker: job %s: %w", c.Job.ID, err))
		p.releaseBestEffort(c, p.now())
		return
	}

	state := p.states.get(c.Job.ID)
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

	switch outcome.Status {
	case retry.StatusSucceeded:
		p.states.delete(c.Job.ID)
		p.ackBestEffort(c)
		p.statusFn(ctx, c.Job.MessageID, outcome.Status, p.now())

	case retry.StatusFailed, retry.StatusExhausted:
		p.handleTerminalFailure(loaded, state, outcome.Status, c.Job.ID)
		p.states.delete(c.Job.ID)
		p.ackBestEffort(c)
		p.statusFn(ctx, c.Job.MessageID, outcome.Status, time.Time{})

	case retry.StatusRetryable:
		if outcome.Schedule == nil {
			p.onError(fmt.Errorf("worker: job %s: retryable outcome missing schedule", c.Job.ID))
			p.releaseBestEffort(c, p.now())
			return
		}
		p.releaseBestEffort(c, outcome.Schedule.NextRetryAt)
		p.statusFn(ctx, c.Job.MessageID, outcome.Status, time.Time{})

	default:
		p.onError(fmt.Errorf("worker: job %s: unrecognized lifecycle status %v", c.Job.ID, outcome.Status))
		p.releaseBestEffort(c, p.now())
	}
}

// handleAttemptError covers the non-nil-error paths of Coordinator.Attempt,
// which normal operation never triggers.
func (p *Pool) handleAttemptError(c queue.Claim, err error) {
	switch {
	case errors.Is(err, retry.ErrNotRetryable), errors.Is(err, retry.ErrRetryExhausted):
		// State was already terminal; ack defensively to stop reprocessing.
		p.onError(fmt.Errorf("worker: job %s: reprocessed an already-terminal retry state: %w", c.Job.ID, err))
		p.states.delete(c.Job.ID)
		p.ackBestEffort(c)
	default:
		p.onError(fmt.Errorf("worker: job %s: coordinator attempt error: %w", c.Job.ID, err))
		p.releaseBestEffort(c, p.now())
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
