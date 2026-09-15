package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const testRedisAddr = "localhost:6379"

// requireRedis fails loudly (not skips silently) unless MAILX_SKIP_REDIS_TESTS
// is set, so a normal `go test ./...` proves real Redis integration rather
// than quietly never running it.
func requireRedis(t *testing.T) {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: testRedisAddr})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis integration tests require a real Redis at %s (start it, e.g. `brew services start redis`): %v", testRedisAddr, err)
	}
}

func testNamespace(t *testing.T) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "test-" + hex.EncodeToString(b) + "-" + sanitizeTestName(t.Name())
}

func sanitizeTestName(name string) string {
	out := make([]byte, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, byte(r))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func newTestRedisQueue(t *testing.T, capacity int, lease time.Duration) *RedisQueue {
	t.Helper()
	return newTestRedisQueueNS(t, testNamespace(t), capacity, lease)
}

// newTestRedisQueueNS lets multiple *RedisQueue instances (simulating
// independent processes) share one namespace within a test.
func newTestRedisQueueNS(t *testing.T, ns string, capacity int, lease time.Duration) *RedisQueue {
	t.Helper()
	q, err := NewRedisQueue(RedisConfig{
		Addr:         testRedisAddr,
		Namespace:    ns,
		Capacity:     capacity,
		ClaimLease:   lease,
		PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client := goredis.NewClient(&goredis.Options{Addr: testRedisAddr})
		defer client.Close()
		ctx := context.Background()
		keys := newRedisKeys(ns)
		ids, _ := client.ZRange(ctx, keys.available, 0, -1).Result()
		claimedIDs, _ := client.ZRange(ctx, keys.claimed, 0, -1).Result()
		del := []string{keys.available, keys.claimed}
		for _, id := range append(ids, claimedIDs...) {
			del = append(del, keys.job(id))
		}
		client.Del(ctx, del...)
		_ = q.Close()
	})
	return q
}

func TestRedisConfigValidation(t *testing.T) {
	cases := []RedisConfig{
		{Namespace: "ns", Capacity: 1},                          // missing addr
		{Addr: testRedisAddr, Capacity: 1},                      // missing namespace
		{Addr: testRedisAddr, Namespace: "bad ns", Capacity: 1}, // invalid chars
		{Addr: testRedisAddr, Namespace: "ns", Capacity: 0},     // bad capacity
	}
	for i, c := range cases {
		if _, err := NewRedisQueue(c); err == nil {
			t.Errorf("case %d: expected validation error, got nil", i)
		}
	}
}

func TestRedisConfigStringOmitsPassword(t *testing.T) {
	c := RedisConfig{Addr: "x", Namespace: "ns", Capacity: 1, Password: "supersecret"}
	if s := c.String(); contains(s, "supersecret") {
		t.Fatalf("Config.String leaked password: %s", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// ---------------------------------------------------------- claim lease --

func TestClaimLeaseExpiryAllowsReclamation(t *testing.T) {
	requireRedis(t)
	ns := testNamespace(t)
	q := newTestRedisQueueNS(t, ns, 4, 100*time.Millisecond)
	ctx := context.Background()

	if err := q.Enqueue(ctx, Job{ID: "abandoned", MessageID: "m"}); err != nil {
		t.Fatal(err)
	}
	first, err := q.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the owning worker dying: never Ack/Release. A second,
	// independent client on the same namespace must eventually reclaim it.
	second := newTestRedisQueueNS(t, ns, 4, 100*time.Millisecond)
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reclaimed, err := second.Claim(claimCtx)
	if err != nil {
		t.Fatalf("expected reclamation after lease expiry: %v", err)
	}
	if reclaimed.Job.ID != "abandoned" {
		t.Fatalf("got %+v", reclaimed.Job)
	}
	if reclaimed.Token == first.Token {
		t.Fatal("expected a fresh token on reclaim")
	}
	// The dead worker's stale token must now be rejected.
	if err := q.Ack(ctx, first.Job.ID, first.Token); !errors.Is(err, ErrJobNotClaimed) {
		t.Fatalf("expected stale ack to be rejected, got %v", err)
	}
}

// ------------------------------------------------------- multi-client ----

func TestMultipleClientsExactlyOneOwnerPerClaim(t *testing.T) {
	requireRedis(t)
	ns := testNamespace(t)
	const jobs = 100
	const clients = 5

	seed := newTestRedisQueueNS(t, ns, jobs+10, time.Minute)
	ctx := context.Background()
	for i := 0; i < jobs; i++ {
		if err := seed.Enqueue(ctx, Job{ID: fmt.Sprintf("job-%03d", i), MessageID: "m"}); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu      sync.Mutex
		seen    = map[string]int{}
		claimed int64
		wg      sync.WaitGroup
	)
	for c := 0; c < clients; c++ {
		q := newTestRedisQueueNS(t, ns, jobs+10, time.Minute)
		wg.Add(1)
		go func(q *RedisQueue) {
			defer wg.Done()
			for {
				claimCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				claim, err := q.Claim(claimCtx)
				cancel()
				if err != nil {
					return // no more jobs within the window
				}
				mu.Lock()
				seen[claim.Job.ID]++
				mu.Unlock()
				if err := q.Ack(ctx, claim.Job.ID, claim.Token); err != nil {
					t.Errorf("ack failed for %s: %v", claim.Job.ID, err)
				}
				atomic.AddInt64(&claimed, 1)
			}
		}(q)
	}
	wg.Wait()

	if int(claimed) != jobs {
		t.Fatalf("expected %d claims, got %d", jobs, claimed)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s claimed %d times, want exactly 1", id, n)
		}
	}
}

func TestCapacityRespectedAcrossClients(t *testing.T) {
	requireRedis(t)
	ns := testNamespace(t)
	const capacity = 5

	var wg sync.WaitGroup
	var accepted int64
	for c := 0; c < 3; c++ {
		q := newTestRedisQueueNS(t, ns, capacity, time.Minute)
		wg.Add(1)
		go func(q *RedisQueue, offset int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				id := fmt.Sprintf("c%d-%d", offset, i)
				if err := q.Enqueue(ctx, Job{ID: id, MessageID: "m"}); err == nil {
					atomic.AddInt64(&accepted, 1)
				}
				cancel()
			}
		}(q, c)
	}
	wg.Wait()

	if accepted > capacity {
		t.Fatalf("accepted %d enqueues, capacity is %d", accepted, capacity)
	}
}
