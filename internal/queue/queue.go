// Package queue is MailX's asynchronous work-scheduling boundary between
// message acceptance and delivery execution. It only schedules and owns
// opaque work references — no SMTP, MIME, or retry-policy knowledge here.
package queue

import (
	"context"
	"errors"
	"time"
)

var (
	ErrQueueClosed     = errors.New("queue: closed")
	ErrUnknownJob      = errors.New("queue: unknown job")
	ErrJobNotClaimed   = errors.New("queue: job is not claimed by this token")
	ErrInvalidCapacity = errors.New("queue: capacity must be positive")
)

// Claim is the ownership handle returned by Queue.Claim; Token guards
// Ack/Release against stale or reused ownership. There is no lease
// timeout: an abandoned claim stays claimed until process restart.
type Claim struct {
	Job   Job
	Token uint64
}

// Queue provides at-least-once, not exactly-once, job processing: nothing
// here makes a remote SMTP side effect and Ack atomic, so a crash between
// acceptance and Ack can still cause a duplicate delivery attempt. The
// in-memory implementation also loses all state, including in-flight
// claims, on process restart.
type Queue interface {
	// Enqueue is a no-op if job.ID is already active; blocks at capacity.
	Enqueue(ctx context.Context, job Job) error

	// Claim blocks until a job is available, returning exclusive ownership.
	Claim(ctx context.Context) (Claim, error)

	// Ack completes a claimed job and frees one unit of capacity.
	Ack(ctx context.Context, id string, token uint64) error

	// Release returns a claimed job to the pool, claimable again at
	// availableAt. Capacity is not freed; the caller decides availableAt.
	Release(ctx context.Context, id string, token uint64, availableAt time.Time) error

	// Close stops new Enqueue/Claim calls; in-flight claims may still be
	// Ack'd or Released. Idempotent.
	Close() error
}
