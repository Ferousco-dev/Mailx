package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
	"github.com/redis/go-redis/v9"
)

// tenantStore adds TenantLookup to the suppression-aware fake store.
type tenantStore struct {
	*suppStore
	tenant string
	err    error
}

func (s *tenantStore) MessageTenant(context.Context, string) (string, error) {
	return s.tenant, s.err
}

// fakePermits is an in-memory Permits whose refusals and failures a test controls.
type fakePermits struct {
	mu          sync.Mutex
	held        map[string]map[string]bool
	limit       map[string]int // key -> limit override; absent = the requested limit
	err         error
	acquires    int
	releases    int
	releaseErrs int // releases observed with an already-canceled context
}

func newFakePermits() *fakePermits {
	return &fakePermits{held: map[string]map[string]bool{}, limit: map[string]int{}}
}

func (f *fakePermits) TenantPermitKey(t string) string      { return "t:" + t }
func (f *fakePermits) DestinationPermitKey(d string) string { return "d:" + d }

func (f *fakePermits) Acquire(ctx context.Context, key string, limit int, _ time.Duration, holder string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires++
	if f.err != nil {
		return false, f.err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if l, ok := f.limit[key]; ok {
		limit = l
	}
	if f.held[key] == nil {
		f.held[key] = map[string]bool{}
	}
	if f.held[key][holder] {
		return true, nil
	}
	if len(f.held[key]) >= limit {
		return false, nil
	}
	f.held[key][holder] = true
	return true, nil
}

func (f *fakePermits) Release(ctx context.Context, key, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	if ctx.Err() != nil {
		f.releaseErrs++
	}
	delete(f.held[key], holder)
	return nil
}

func (f *fakePermits) heldTotal() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.held {
		n += len(h)
	}
	return n
}

func (f *fakePermits) set(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }

var testPermitPolicy = PermitPolicy{TenantLimit: 2, DestinationLimit: 2, TTL: time.Minute, JitterPercent: 10, Deferral: 50 * time.Millisecond}

func attemptsFor(store *suppStore, id string) int {
	store.fakeOutcomeStore.mu.Lock()
	defer store.fakeOutcomeStore.mu.Unlock()
	return len(store.fakeOutcomeStore.attempts[id])
}

// A refused permit defers the job: no SMTP, NO delivery attempt recorded, and once
// capacity returns the message is delivered exactly once and every permit is given back.
func TestPermitRefusalDefersWithoutAttemptThenDelivers(t *testing.T) {
	store := &tenantStore{suppStore: newSuppStore(), tenant: "tenant-1"}
	rig, mk := permitRig(t, store)
	perm := newFakePermits()
	perm.set(func() { perm.limit["t:tenant-1"] = 0 }) // tenant is at its limit
	p := mk(2, WithPermits(perm, testPermitPolicy))
	rig.message("m1", "<bob@dest.example>")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	time.Sleep(400 * time.Millisecond) // several deferral cycles
	if rig.mx.Conns.Load() != 0 || attemptsFor(store.suppStore, "m1") != 0 {
		t.Fatalf("a deferred job must not touch SMTP or record an attempt: conns=%d attempts=%d", rig.mx.Conns.Load(), attemptsFor(store.suppStore, "m1"))
	}
	if perm.heldTotal() != 0 {
		t.Fatal("a refused job holds no permit")
	}
	perm.set(func() { delete(perm.limit, "t:tenant-1") }) // capacity returns
	deadline := time.Now().Add(10 * time.Second)
	for len(rig.mx.Messages()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(rig.mx.Messages()) != 1 || attemptsFor(store.suppStore, "m1") != 1 {
		t.Fatalf("delivered %d, attempts %d, want exactly 1 each", len(rig.mx.Messages()), attemptsFor(store.suppStore, "m1"))
	}
	if perm.heldTotal() != 0 {
		t.Fatalf("%d permits leaked after delivery", perm.heldTotal())
	}
}

// permitRig is a suppression rig whose pools see a tenant-aware outcome store.
func permitRig(t *testing.T, store *tenantStore) (*suppRig, func(workers int, opts ...Option) *Pool) {
	t.Helper()
	rig, mk := newSuppRig(t, smtptest.Options{}, false, store.suppStore)
	rig.outcomes = store
	return rig, mk
}

// The destination limit is enforced independently of the tenant.
func TestDestinationPermitRefusalDefersAndReleasesTenantPermit(t *testing.T) {
	store := &tenantStore{suppStore: newSuppStore(), tenant: "tenant-1"}
	rig, mk := permitRig(t, store)
	perm := newFakePermits()
	perm.set(func() { perm.limit["d:dest.example"] = 0 })
	p := mk(1, WithPermits(perm, testPermitPolicy))
	rig.message("m1", "<bob@dest.example>")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	if perm.heldTotal() != 0 {
		t.Fatal("the tenant permit must be given back when the destination permit is refused")
	}
	if rig.mx.Conns.Load() != 0 || attemptsFor(store.suppStore, "m1") != 0 {
		t.Fatal("deferred job must not attempt")
	}
	perm.set(func() { delete(perm.limit, "d:dest.example") })
	deadline := time.Now().Add(10 * time.Second)
	for len(rig.mx.Messages()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(rig.mx.Messages()) != 1 || perm.heldTotal() != 0 {
		t.Fatalf("delivered=%d held=%d", len(rig.mx.Messages()), perm.heldTotal())
	}
}

// Fail closed: permit state unknown (limiter down / tenant lookup failing) means defer, never send unmetered.
func TestUnknownPermitStateDefersAndRecovers(t *testing.T) {
	for _, mode := range []string{"limiter down", "tenant lookup failing"} {
		t.Run(mode, func(t *testing.T) {
			store := &tenantStore{suppStore: newSuppStore(), tenant: "tenant-1"}
			rig, mk := permitRig(t, store)
			perm := newFakePermits()
			pol := testPermitPolicy
			if mode == "limiter down" {
				perm.set(func() { perm.err = errors.New("redis: down") })
			} else {
				store.err = errors.New("postgres: down")
			}
			p := mk(1, WithPermits(perm, pol))
			rig.message("m1", "<bob@dest.example>")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { _ = p.Run(ctx); close(done) }()
			time.Sleep(300 * time.Millisecond)
			cancel()
			<-done
			if rig.mx.Conns.Load() != 0 || attemptsFor(store.suppStore, "m1") != 0 {
				t.Fatalf("unknown permit state must not send: conns=%d attempts=%d", rig.mx.Conns.Load(), attemptsFor(store.suppStore, "m1"))
			}
			if rig.base.Len() != 1 {
				t.Fatalf("the job must remain queued for later, not be lost or acked: len=%d", rig.base.Len())
			}
		})
	}
}

// A suppressed-only job never takes a permit, and a job with a canceled context still
// releases (fresh context).
func TestSuppressedJobTakesNoPermit(t *testing.T) {
	store := &tenantStore{suppStore: newSuppStore("bob@dest.example"), tenant: "tenant-1"}
	rig, mk := permitRig(t, store)
	perm := newFakePermits()
	p := mk(1, WithPermits(perm, testPermitPolicy))
	rig.message("m1", "<bob@dest.example>")
	runUntil(t, p, func() bool { return rig.base.Len() == 0 && len(store.recordedCalls()) == 1 })
	if perm.acquires != 0 {
		t.Fatalf("a suppressed job acquired %d permits", perm.acquires)
	}
}

func TestPermitReleaseSurvivesCanceledContext(t *testing.T) {
	store := &tenantStore{suppStore: newSuppStore(), tenant: "tenant-1"}
	_, mk := permitRig(t, store)
	perm := newFakePermits()
	p := mk(1, WithPermits(perm, testPermitPolicy))
	ctx, cancel := context.WithCancel(context.Background())
	claim := queue.Claim{Job: queue.Job{ID: "job-x", MessageID: "x"}}
	done, ok := p.acquirePermits(ctx, claim, "dest.example")
	if !ok || perm.heldTotal() != 2 {
		t.Fatalf("ok=%v held=%d, want both permits", ok, perm.heldTotal())
	}
	cancel() // shutdown arrives mid-attempt
	done()
	if perm.heldTotal() != 0 || perm.releaseErrs != 0 {
		t.Fatalf("held=%d releasesWithCanceledCtx=%d: release must use a fresh context", perm.heldTotal(), perm.releaseErrs)
	}
}

// ---- real Redis: fairness of completion and crash recovery ----

func realPermitStore(t *testing.T) *ratelimit.Store {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis is required for these tests: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	ns := "mailx:workertest:" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		ctx := context.Background()
		it := rdb.Scan(ctx, 0, ns+"*", 1000).Iterator()
		for it.Next(ctx) {
			rdb.Del(ctx, it.Val())
		}
		_ = rdb.Close()
	})
	return ratelimit.NewStore(rdb, ns)
}

// Many messages, one tenant, limit 2: everything is eventually delivered exactly once,
// nothing is lost or duplicated, and no permit leaks.
func TestRealRedisPermitsDeliverEverythingUnderTheLimit(t *testing.T) {
	perm := realPermitStore(t)
	store := &tenantStore{suppStore: newSuppStore(), tenant: "tenant-1"}
	rig, mk := permitRig(t, store)
	pol := PermitPolicy{TenantLimit: 2, DestinationLimit: 2, TTL: time.Minute, JitterPercent: 20, Deferral: 30 * time.Millisecond}
	p := mk(8, WithPermits(perm, pol))
	const n = 12
	for i := 0; i < n; i++ {
		rig.message(fmt.Sprintf("m%d", i), fmt.Sprintf("<u%d@dest.example>", i))
	}
	runUntil(t, p, func() bool { return len(rig.mx.Messages()) == n && rig.base.Len() == 0 })
	for i := 0; i < n; i++ {
		if a := attemptsFor(store.suppStore, fmt.Sprintf("m%d", i)); a != 1 {
			t.Fatalf("m%d has %d attempts, want exactly 1", i, a)
		}
	}
	for _, k := range []string{perm.TenantPermitKey("tenant-1"), perm.DestinationPermitKey("dest.example")} {
		if h, err := perm.Held(context.Background(), k); err != nil || h != 0 {
			t.Fatalf("permit %s leaked: held=%d err=%v", k, h, err)
		}
	}
}

// A worker that died while holding the tenant's only permit does not block the tenant
// forever: the permit expires by TTL and the deferred message is then delivered.
func TestCrashedWorkerPermitIsRecoveredByTTL(t *testing.T) {
	perm := realPermitStore(t)
	store := &tenantStore{suppStore: newSuppStore(), tenant: "tenant-1"}
	rig, mk := permitRig(t, store)
	ctx := context.Background()
	got, err := perm.Acquire(ctx, perm.TenantPermitKey("tenant-1"), 1, 1500*time.Millisecond, "dead-worker-job")
	if err != nil || !got {
		t.Fatalf("setup: %v %v", got, err)
	}
	pol := PermitPolicy{TenantLimit: 1, DestinationLimit: 4, TTL: time.Minute, JitterPercent: 10, Deferral: 200 * time.Millisecond}
	p := mk(2, WithPermits(perm, pol))
	rig.message("m1", "<bob@dest.example>")

	start := time.Now()
	runUntil(t, p, func() bool { return len(rig.mx.Messages()) == 1 && rig.base.Len() == 0 })
	if waited := time.Since(start); waited < 800*time.Millisecond {
		t.Fatalf("delivered after %s: the dead worker's permit should have blocked it until its TTL", waited)
	}
	if attemptsFor(store.suppStore, "m1") != 1 {
		t.Fatalf("attempts = %d", attemptsFor(store.suppStore, "m1"))
	}
}
