// Redis-backed Queue implementation.
//
// State machine: a job is at all times represented by a HASH
// (mailx:queue:<ns>:job:<id>, the Job fields + owning token) and exactly one
// membership in one of two ZSETs: "available" (score = AvailableAt) or
// "claimed" (score = lease expiry). Enqueue/Claim/Ack/Release are each one
// Lua script (redis_scripts.go) so the read-then-write transition is atomic
// across every process sharing the namespace — see STEP 1-9 of the v0.16
// design notes in the PR description for the full rationale.
//
// Unlike MemoryQueue, Redis can outlive the MailX process that claimed a
// job. So a claim carries a lease: if the owning worker never Acks or
// Releases before the lease expires, Claim itself reclaims it (bounded,
// atomic, no per-job timer/goroutine). This gives liveness, NOT
// exactly-once — a worker that is merely slow (not dead) can have its job
// reclaimed and delivered twice; callers must size ClaimLease above the
// slowest realistic delivery attempt. This is documented, not solved, here.
//
// retry.State (attempt history/backoff position) is NOT stored in Redis:
// it remains worker.Pool-owned, in-memory, per the existing v0.14 contract.
// A worker process restart still loses retry.State exactly as it does
// today; RedisQueue surviving the restart does not change that.
package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig configures one RedisQueue. Password is intentionally not
// echoed by String, matching internal/database.Config.
type RedisConfig struct {
	Addr      string
	Username  string
	Password  string
	DB        int
	Namespace string
	Capacity  int
	// ClaimLease bounds how long a Claim is honored before another worker
	// may reclaim it. Must exceed the slowest realistic delivery attempt.
	ClaimLease time.Duration
	// PollInterval bounds how long Claim/Enqueue wait between polls when
	// blocked; Redis has no server-side blocking primitive that fits this
	// queue's multi-condition wake-up (capacity freed, job due, lease
	// expired), so polling is the deliberate distributed wake-up strategy.
	PollInterval time.Duration
}

const (
	defaultClaimLease   = 2 * time.Minute
	defaultPollInterval = 200 * time.Millisecond
	maxPollInterval     = 2 * time.Second
	reclaimBatchLimit   = 50
)

func (c RedisConfig) String() string {
	return fmt.Sprintf("queue.RedisConfig{Addr:%s DB:%d Namespace:%s Capacity:%d ClaimLease:%s PollInterval:%s}",
		c.Addr, c.DB, c.Namespace, c.Capacity, c.ClaimLease, c.PollInterval)
}

func (c RedisConfig) normalized() (RedisConfig, error) {
	if c.Addr == "" {
		return RedisConfig{}, errors.New("queue: redis addr is empty")
	}
	if !validNamespace(c.Namespace) {
		return RedisConfig{}, errors.New("queue: redis namespace must be non-empty and contain only [A-Za-z0-9-_.]")
	}
	if c.Capacity <= 0 {
		return RedisConfig{}, ErrInvalidCapacity
	}
	if c.ClaimLease < 0 {
		return RedisConfig{}, errors.New("queue: claim lease must not be negative")
	}
	if c.ClaimLease == 0 {
		c.ClaimLease = defaultClaimLease
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	return c, nil
}

// RedisQueue is a distributed Queue: multiple processes may share one
// namespace safely. Close only shuts down this process's client — it does
// not affect other processes using the same Redis namespace.
type RedisQueue struct {
	client   *redis.Client
	keys     redisKeys
	capacity int
	lease    time.Duration
	poll     time.Duration
	now      func() time.Time
	closed   chan struct{}
}

var _ Queue = (*RedisQueue)(nil)

func NewRedisQueue(cfg RedisConfig) (*RedisQueue, error) {
	normalized, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(&redis.Options{
		Addr:     normalized.Addr,
		Username: normalized.Username,
		Password: normalized.Password,
		DB:       normalized.DB,
	})
	return &RedisQueue{
		client:   client,
		keys:     newRedisKeys(normalized.Namespace),
		capacity: normalized.Capacity,
		lease:    normalized.ClaimLease,
		poll:     normalized.PollInterval,
		now:      func() time.Time { return time.Now().UTC() },
		closed:   make(chan struct{}),
	}, nil
}

func (q *RedisQueue) isClosed() bool {
	select {
	case <-q.closed:
		return true
	default:
		return false
	}
}

func (q *RedisQueue) Enqueue(ctx context.Context, job Job) error {
	if err := job.validate(); err != nil {
		return err
	}
	normalized := job
	if normalized.EnqueuedAt.IsZero() {
		normalized.EnqueuedAt = q.now()
	}
	if normalized.AvailableAt.IsZero() {
		normalized.AvailableAt = normalized.EnqueuedAt
	}

	for {
		if q.isClosed() {
			return ErrQueueClosed
		}
		res, err := enqueueScript.Run(ctx, q.client,
			[]string{q.keys.available, q.keys.claimed, q.keys.job(normalized.ID)},
			normalized.ID, normalized.MessageID, toMillis(normalized.EnqueuedAt), toMillis(normalized.AvailableAt), q.capacity,
		).Result()
		if err != nil {
			return fmt.Errorf("queue: redis enqueue: %w", err)
		}
		switch n := res.(int64); n {
		case 1, 0:
			return nil // enqueued, or already-active duplicate (no-op)
		case -1:
			// at capacity; wait for an Ack/Release to free a slot.
			if err := q.sleep(ctx, q.poll); err != nil {
				return err
			}
		default:
			return fmt.Errorf("queue: redis enqueue: unexpected script result %d", n)
		}
	}
}

func (q *RedisQueue) Claim(ctx context.Context) (Claim, error) {
	for {
		if q.isClosed() {
			return Claim{}, ErrQueueClosed
		}
		now := q.now()
		res, err := claimScript.Run(ctx, q.client,
			[]string{q.keys.available, q.keys.claimed},
			toMillis(now), q.lease.Milliseconds(), q.keys.jobPrefix, q.keys.tokenSeq, reclaimBatchLimit,
		).Result()
		if err != nil {
			return Claim{}, fmt.Errorf("queue: redis claim: %w", err)
		}
		fields, ok := res.([]interface{})
		if !ok {
			return Claim{}, fmt.Errorf("queue: redis claim: unexpected script result %T", res)
		}
		if len(fields) == 0 {
			wait, err := q.waitHint(ctx, now)
			if err != nil {
				return Claim{}, err
			}
			if err := q.sleep(ctx, wait); err != nil {
				return Claim{}, err
			}
			continue
		}
		return parseClaim(fields)
	}
}

func (q *RedisQueue) Ack(ctx context.Context, id string, token uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := ackScript.Run(ctx, q.client, []string{q.keys.claimed}, id, strconv.FormatUint(token, 10), q.keys.jobPrefix).Result()
	if err != nil {
		return fmt.Errorf("queue: redis ack: %w", err)
	}
	switch res.(int64) {
	case 1:
		return nil
	case -1:
		return ErrUnknownJob
	case -2:
		return ErrJobNotClaimed
	default:
		return fmt.Errorf("queue: redis ack: unexpected script result %v", res)
	}
}

func (q *RedisQueue) Release(ctx context.Context, id string, token uint64, availableAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := releaseScript.Run(ctx, q.client,
		[]string{q.keys.available, q.keys.claimed},
		id, strconv.FormatUint(token, 10), toMillis(availableAt), q.keys.jobPrefix,
	).Result()
	if err != nil {
		return fmt.Errorf("queue: redis release: %w", err)
	}
	switch res.(int64) {
	case 1:
		return nil
	case -1:
		return ErrUnknownJob
	case -2:
		return ErrJobNotClaimed
	default:
		return fmt.Errorf("queue: redis release: unexpected script result %v", res)
	}
}

// Close shuts down this process's Redis client only; other processes
// sharing the namespace are unaffected and keep processing jobs.
func (q *RedisQueue) Close() error {
	if q.isClosed() {
		return nil
	}
	close(q.closed)
	return q.client.Close()
}

// waitHint looks at the earliest available-job score to avoid polling
// faster than necessary when the next due job is known; it is a read-only
// hint, not part of any atomic transition.
func (q *RedisQueue) waitHint(ctx context.Context, now time.Time) (time.Duration, error) {
	top, err := q.client.ZRangeWithScores(ctx, q.keys.available, 0, 0).Result()
	if err != nil {
		return 0, fmt.Errorf("queue: redis peek available: %w", err)
	}
	if len(top) == 0 {
		return q.poll, nil
	}
	dueAt := fromMillis(int64(top[0].Score))
	wait := dueAt.Sub(now)
	if wait <= 0 {
		return 0, nil
	}
	if wait > maxPollInterval {
		wait = maxPollInterval
	}
	return wait, nil
}

func (q *RedisQueue) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closed:
		return ErrQueueClosed
	case <-timer.C:
		return nil
	}
}

func parseClaim(fields []interface{}) (Claim, error) {
	if len(fields) != 5 {
		return Claim{}, fmt.Errorf("queue: redis claim: expected 5 fields, got %d", len(fields))
	}
	id, _ := fields[0].(string)
	tokenStr, _ := fields[1].(string)
	messageID, _ := fields[2].(string)
	enqueuedAtStr, _ := fields[3].(string)
	availableAtStr, _ := fields[4].(string)

	token, err := strconv.ParseUint(tokenStr, 10, 64)
	if err != nil {
		return Claim{}, fmt.Errorf("queue: redis claim: parse token: %w", err)
	}
	enqueuedAtMs, err := strconv.ParseInt(enqueuedAtStr, 10, 64)
	if err != nil {
		return Claim{}, fmt.Errorf("queue: redis claim: parse enqueued_at: %w", err)
	}
	availableAtMs, err := strconv.ParseInt(availableAtStr, 10, 64)
	if err != nil {
		return Claim{}, fmt.Errorf("queue: redis claim: parse available_at: %w", err)
	}

	return Claim{
		Job: Job{
			ID:          id,
			MessageID:   messageID,
			EnqueuedAt:  fromMillis(enqueuedAtMs),
			AvailableAt: fromMillis(availableAtMs),
		},
		Token: token,
	}, nil
}

func toMillis(t time.Time) int64    { return t.UTC().UnixMilli() }
func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
