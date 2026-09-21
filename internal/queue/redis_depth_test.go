package queue

import (
	"context"
	"testing"
	"time"
)

func TestRedisPingAndDepthTrackQueueTransitions(t *testing.T) {
	q := newTestRedisQueue(t, 10, time.Minute)
	ctx := context.Background()
	if err := q.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	depth := func(want int64) {
		t.Helper()
		got, err := q.Depth(ctx)
		if err != nil || got != want {
			t.Fatalf("depth=%d err=%v, want %d", got, err, want)
		}
	}
	depth(0)
	for _, id := range []string{"a", "b"} {
		if err := q.Enqueue(ctx, Job{ID: id, MessageID: id}); err != nil {
			t.Fatal(err)
		}
	}
	depth(2) // available
	c, err := q.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	depth(2) // claimed jobs still count
	if err := q.Release(ctx, c.Job.ID, c.Token, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	depth(2) // released back to available
	c2, err := q.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(ctx, c2.Job.ID, c2.Token); err != nil {
		t.Fatal(err)
	}
	depth(1)
	// Reading depth must not have mutated anything: a second read is identical.
	depth(1)
}

func TestRedisPingFailsWhenClosedContext(t *testing.T) {
	q := newTestRedisQueue(t, 1, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Ping(ctx); err == nil {
		t.Fatal("cancelled ping should fail")
	}
	if _, err := q.Depth(ctx); err == nil {
		t.Fatal("cancelled depth should fail")
	}
}
