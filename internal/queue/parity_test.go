package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// parityQueue is the subset of Queue behavior both MemoryQueue and
// RedisQueue must satisfy identically. Strict FIFO tie-break among equal
// AvailableAt values is intentionally NOT included here: RedisQueue does
// not guarantee it (documented in redis.go), only MemoryQueue does
// (covered separately in memory_test.go).
func runParitySuite(t *testing.T, newQ func(t *testing.T, capacity int) Queue) {
	t.Run("EnqueueThenClaim", func(t *testing.T) {
		q := newQ(t, 4)
		ctx := context.Background()
		if err := q.Enqueue(ctx, Job{ID: "j1", MessageID: "m1"}); err != nil {
			t.Fatal(err)
		}
		c, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c.Job.ID != "j1" || c.Job.MessageID != "m1" {
			t.Fatalf("got %+v", c.Job)
		}
	})

	t.Run("DuplicateActiveJobIDIsNoOp", func(t *testing.T) {
		q := newQ(t, 4)
		ctx := context.Background()
		if err := q.Enqueue(ctx, Job{ID: "dup", MessageID: "m1"}); err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(ctx, Job{ID: "dup", MessageID: "m2"}); err != nil {
			t.Fatal(err)
		}
		c, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c.Job.MessageID != "m1" {
			t.Fatalf("duplicate enqueue must not overwrite first job; got MessageID %q", c.Job.MessageID)
		}
		claimCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if _, err := q.Claim(claimCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected no second job, got %v", err)
		}
	})

	t.Run("DelayedJobNotClaimableEarly", func(t *testing.T) {
		q := newQ(t, 4)
		ctx := context.Background()
		if err := q.Enqueue(ctx, Job{ID: "delayed", MessageID: "m", AvailableAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		claimCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if _, err := q.Claim(claimCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected delayed job to stay unclaimable, got %v", err)
		}
	})

	t.Run("AckRemovesJob", func(t *testing.T) {
		q := newQ(t, 4)
		ctx := context.Background()
		if err := q.Enqueue(ctx, Job{ID: "ackme", MessageID: "m"}); err != nil {
			t.Fatal(err)
		}
		c, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
			t.Fatal(err)
		}
		if err := q.Ack(ctx, c.Job.ID, c.Token); !errors.Is(err, ErrUnknownJob) {
			t.Fatalf("expected ErrUnknownJob on double-ack, got %v", err)
		}
	})

	t.Run("StaleTokenRejectedOnAckAndRelease", func(t *testing.T) {
		q := newQ(t, 4)
		ctx := context.Background()
		if err := q.Enqueue(ctx, Job{ID: "stale", MessageID: "m"}); err != nil {
			t.Fatal(err)
		}
		c, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		badToken := c.Token + 999
		if err := q.Ack(ctx, c.Job.ID, badToken); !errors.Is(err, ErrJobNotClaimed) {
			t.Fatalf("expected ErrJobNotClaimed, got %v", err)
		}
		if err := q.Release(ctx, c.Job.ID, badToken, time.Now()); !errors.Is(err, ErrJobNotClaimed) {
			t.Fatalf("expected ErrJobNotClaimed, got %v", err)
		}
		if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
			t.Fatalf("real token must still work: %v", err)
		}
	})

	t.Run("ReleaseMakesJobClaimableAgainWithFreshToken", func(t *testing.T) {
		q := newQ(t, 4)
		ctx := context.Background()
		if err := q.Enqueue(ctx, Job{ID: "requeue", MessageID: "m"}); err != nil {
			t.Fatal(err)
		}
		first, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Release(ctx, first.Job.ID, first.Token, time.Now()); err != nil {
			t.Fatal(err)
		}
		second, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if second.Token == first.Token {
			t.Fatalf("expected a fresh token on reclaim, got same token %d", second.Token)
		}
		if err := q.Ack(ctx, first.Job.ID, first.Token); !errors.Is(err, ErrJobNotClaimed) {
			t.Fatalf("old token must be rejected after reclaim, got %v", err)
		}
	})

	t.Run("UnknownJobIDRejected", func(t *testing.T) {
		q := newQ(t, 4)
		ctx := context.Background()
		if err := q.Ack(ctx, "never-enqueued", 1); !errors.Is(err, ErrUnknownJob) {
			t.Fatalf("expected ErrUnknownJob, got %v", err)
		}
	})

	t.Run("CapacityBlocksEnqueueUntilAckFreesSlot", func(t *testing.T) {
		q := newQ(t, 1)
		ctx := context.Background()
		if err := q.Enqueue(ctx, Job{ID: "full", MessageID: "m"}); err != nil {
			t.Fatal(err)
		}
		blockedCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if err := q.Enqueue(blockedCtx, Job{ID: "blocked", MessageID: "m"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected Enqueue to block at capacity, got %v", err)
		}
		c, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Ack(ctx, c.Job.ID, c.Token); err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(ctx, Job{ID: "blocked", MessageID: "m"}); err != nil {
			t.Fatalf("expected slot freed after Ack: %v", err)
		}
	})

	t.Run("ContextCancellationDuringClaimWait", func(t *testing.T) {
		q := newQ(t, 4)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		if _, err := q.Claim(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
}

func TestMemoryQueueParity(t *testing.T) {
	runParitySuite(t, func(t *testing.T, capacity int) Queue {
		return mustQueue(t, capacity)
	})
}

func TestRedisQueueParity(t *testing.T) {
	requireRedis(t)
	runParitySuite(t, func(t *testing.T, capacity int) Queue {
		return newTestRedisQueue(t, capacity, 0)
	})
}
