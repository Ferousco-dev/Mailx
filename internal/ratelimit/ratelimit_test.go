package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func redisAddr() string {
	if a := os.Getenv("REDIS_ADDR"); a != "" {
		return a
	}
	return "localhost:6379"
}

func newClient(t testing.TB) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: redisAddr()})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis is required for these tests (they fail rather than skip): %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// testStore returns a Store in a private namespace that is wiped afterwards, plus a
// controllable clock (microseconds).
func testStore(t testing.TB) (*Store, *atomic.Int64) {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	ns := "mailx:limittest:" + hex.EncodeToString(b[:])
	c := newClient(t)
	var now atomic.Int64
	now.Store(1_800_000_000_000_000)
	s := NewStore(c, ns).WithClock(now.Load)
	t.Cleanup(func() {
		ctx := context.Background()
		iter := c.Scan(ctx, 0, ns+"*", 1000).Iterator()
		for iter.Next(ctx) {
			c.Del(ctx, iter.Val())
		}
	})
	return s, &now
}

func advance(now *atomic.Int64, d time.Duration) { now.Add(d.Microseconds()) }

func allow(t testing.TB, s *Store, b ...Bucket) Decision {
	t.Helper()
	d, err := s.Allow(context.Background(), b...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGCRABurstRefillAndExactRetryAfter(t *testing.T) {
	s, now := testStore(t)
	b := Bucket{Key: s.TenantRequestKey("t1"), Rate: 10, Burst: 5, Cost: 1}
	for i := 0; i < 5; i++ { // the full burst is allowed at once
		if d := allow(t, s, b); !d.Allowed {
			t.Fatalf("request %d of the burst was refused: %+v", i+1, d)
		}
	}
	d := allow(t, s, b)
	if d.Allowed || d.RetryAfter != 100*time.Millisecond || d.Impossible {
		t.Fatalf("6th request: %+v (one token at 10/s is exactly 100ms away)", d)
	}
	// Retry-After is truthful: waiting less is still refused, waiting exactly that long succeeds.
	advance(now, 99*time.Millisecond)
	if d = allow(t, s, b); d.Allowed || d.RetryAfter != time.Millisecond {
		t.Fatalf("after 99ms: %+v", d)
	}
	advance(now, time.Millisecond)
	if d = allow(t, s, b); !d.Allowed {
		t.Fatalf("after the advertised wait: %+v", d)
	}
	// A long idle period refills to the burst and never beyond it.
	advance(now, time.Hour)
	for i := 0; i < 5; i++ {
		if d = allow(t, s, b); !d.Allowed {
			t.Fatalf("refilled request %d: %+v", i+1, d)
		}
	}
	if d = allow(t, s, b); d.Allowed {
		t.Fatal("the bucket accumulated more than its burst while idle")
	}
}

func TestGCRACostAndImpossibleCost(t *testing.T) {
	s, now := testStore(t)
	b := Bucket{Key: s.TenantRecipientKey("t1"), Rate: 20, Burst: 100, Cost: 40}
	if !allow(t, s, b).Allowed || !allow(t, s, b).Allowed {
		t.Fatal("two 40-recipient messages fit a burst of 100")
	}
	d := allow(t, s, b) // 80 used, 20 left, needs 40 => 20 more tokens => 1s
	if d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("%+v", d)
	}
	advance(now, time.Second)
	if !allow(t, s, b).Allowed {
		t.Fatal("refused after the advertised wait")
	}
	// A cost larger than the burst can never succeed: reported as Impossible, not as a wait.
	big := Bucket{Key: s.TenantRecipientKey("t2"), Rate: 20, Burst: 100, Cost: 101}
	if d = allow(t, s, big); d.Allowed || !d.Impossible {
		t.Fatalf("%+v", d)
	}
}

func TestMultiBucketIsAtomicAndKeysCannotAddTenantCapacity(t *testing.T) {
	s, _ := testStore(t)
	tenant := Bucket{Key: s.TenantRequestKey("t1"), Rate: 1, Burst: 10, Cost: 1}
	key := func(id string) Bucket { return Bucket{Key: s.APIKeyGuardKey("t1", id), Rate: 1, Burst: 10, Cost: 1} }
	// Two API keys of ONE tenant: together they cannot exceed the tenant's burst of 10.
	allowed := 0
	for i := 0; i < 20; i++ {
		id := []string{"keyA", "keyB"}[i%2]
		if allow(t, s, tenant, key(id)).Allowed {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("a second API key must not add capacity: %d of 20 allowed, want exactly the tenant burst", allowed)
	}
	// Atomicity: a refusal by one bucket charges NO bucket. Fresh buckets: tenant burst 2, key burst 5.
	s2, _ := testStore(t)
	tn := Bucket{Key: s2.TenantRequestKey("t9"), Rate: 1, Burst: 2, Cost: 1}
	kb := Bucket{Key: s2.APIKeyGuardKey("t9", "k"), Rate: 1, Burst: 5, Cost: 1}
	allow(t, s2, tn, kb)
	allow(t, s2, tn, kb)
	if d := allow(t, s2, tn, kb); d.Allowed || d.Denied != 0 {
		t.Fatalf("the tenant bucket must refuse: %+v", d)
	}
	// The refused request must not have consumed the key bucket: 3 of its 5 tokens remain.
	if d := allow(t, s2, Bucket{Key: kb.Key, Rate: 1, Burst: 5, Cost: 3}); !d.Allowed {
		t.Fatalf("a refused request charged the key bucket: %+v", d)
	}
	// The key guard also contains a single noisy key without exhausting the tenant.
	s3, _ := testStore(t)
	tn3 := Bucket{Key: s3.TenantRequestKey("t"), Rate: 1, Burst: 20, Cost: 1}
	guard := func(id string) Bucket { return Bucket{Key: s3.APIKeyGuardKey("t", id), Rate: 1, Burst: 5, Cost: 1} }
	n := 0
	for i := 0; i < 20; i++ {
		if allow(t, s3, tn3, guard("noisy")).Allowed {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("the noisy key was allowed %d requests, want its guard burst of 5", n)
	}
	if !allow(t, s3, tn3, guard("other")).Allowed {
		t.Fatal("another key of the same tenant must still work")
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	s, _ := testStore(t)
	a := Bucket{Key: s.TenantRequestKey("tenantA"), Rate: 1, Burst: 3, Cost: 1}
	b := Bucket{Key: s.TenantRequestKey("tenantB"), Rate: 1, Burst: 3, Cost: 1}
	for i := 0; i < 3; i++ {
		allow(t, s, a)
	}
	if allow(t, s, a).Allowed {
		t.Fatal("A should be exhausted")
	}
	for i := 0; i < 3; i++ {
		if !allow(t, s, b).Allowed {
			t.Fatalf("tenant A's traffic consumed tenant B's allowance at request %d", i+1)
		}
	}
}

func TestStateSurvivesNewStoresAndProcessesShareOneTruth(t *testing.T) {
	// Two independent "processes" (separate clients and Store instances) share one bucket.
	s1, _ := testStore(t)
	c2 := newClient(t)
	s2 := NewStore(c2, s1.ns) // the real server clock, like a second MailX process
	s1 = NewStore(newClient(t), s1.ns)
	b := func(s *Store) Bucket {
		return Bucket{Key: s.TenantRequestKey("shared"), Rate: 0.001, Burst: 100, Cost: 1}
	}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 400; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := []*Store{s1, s2}[i%2]
			d, err := s.Allow(context.Background(), b(s))
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 100 {
		t.Fatalf("two processes allowed %d, want exactly the burst of 100 (distributed atomicity)", allowed.Load())
	}
	// "Restart": brand-new stores see the same, still-exhausted bucket (limits do not vanish).
	s3 := NewStore(newClient(t), s1.ns)
	if d, _ := s3.Allow(context.Background(), b(s3)); d.Allowed {
		t.Fatal("a restarted process saw a fresh bucket: limits must not disappear on restart")
	}
	t.Cleanup(func() { c := newClient(t); c.Del(context.Background(), s1.TenantRequestKey("shared")) })
}

func TestBucketsCarryTTLAndLeaveNothingBehind(t *testing.T) {
	c := newClient(t)
	var b6 [6]byte
	_, _ = rand.Read(b6[:])
	s := NewStore(c, "mailx:limittest:ttl"+hex.EncodeToString(b6[:]))
	key := s.TenantRequestKey("idle")
	if d, err := s.Allow(context.Background(), Bucket{Key: key, Rate: 1000, Burst: 2, Cost: 1}); err != nil || !d.Allowed {
		t.Fatal(d, err)
	}
	ttl, err := c.PTTL(context.Background(), key).Result()
	if err != nil || ttl <= 0 || ttl > 2*time.Second {
		t.Fatalf("every bucket must expire on its own: ttl=%v err=%v", ttl, err)
	}
	if !waitGone(c, key, 8*time.Second) {
		t.Fatal("an idle tenant left a key behind")
	}
}

func TestRedisFailuresReturnErrorsAndChargeNothing(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer dead.Close()
	s := NewStore(dead, "mailx:limittest:dead").WithCallTimeout(150 * time.Millisecond)
	start := time.Now()
	if _, err := s.Allow(context.Background(), Bucket{Key: "k", Rate: 1, Burst: 1, Cost: 1}); err == nil {
		t.Fatal("an unreachable Redis must be an error, never an implicit allow")
	}
	if _, err := s.Acquire(context.Background(), "p", 1, time.Minute, "h"); err == nil {
		t.Fatal("permit acquire must error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Redis failure was not bounded: %v", time.Since(start))
	}
	// Cancellation is an error too, promptly.
	real, _ := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := real.Allow(ctx, Bucket{Key: real.TenantRequestKey("c"), Rate: 1, Burst: 1, Cost: 1}); err == nil {
		t.Fatal("a canceled context must not produce an allow")
	}
	// Invalid input is refused without touching Redis.
	for _, bad := range []Bucket{{Key: "k", Rate: 0, Burst: 1, Cost: 1}, {Key: "k", Rate: 1, Burst: 0, Cost: 1}, {Key: "k", Rate: 1, Burst: 1, Cost: 0}, {Key: "k", Rate: math.NaN(), Burst: 1, Cost: 1}} {
		if _, err := s.Allow(context.Background(), bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestPermitsBoundConcurrencyAndRecoverFromCrashes(t *testing.T) {
	s, now := testStore(t)
	key := s.TenantPermitKey("t1")
	ctx := context.Background()
	mustAcq := func(holder string, limit int) bool {
		ok, err := s.Acquire(ctx, key, limit, 10*time.Second, holder)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for _, h := range []string{"a", "b", "c"} {
		if !mustAcq(h, 3) {
			t.Fatalf("permit %s refused below the limit", h)
		}
	}
	if mustAcq("d", 3) {
		t.Fatal("a fourth permit was granted at limit 3")
	}
	if !mustAcq("a", 3) { // re-acquire by the same holder refreshes, it does not consume a slot
		t.Fatal("re-acquire by the holder must succeed")
	}
	if n, _ := s.Held(ctx, key); n != 3 {
		t.Fatalf("held=%d", n)
	}
	if err := s.Release(ctx, key, "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, key, "b"); err != nil { // idempotent
		t.Fatal(err)
	}
	if !mustAcq("d", 3) {
		t.Fatal("a released permit must be reusable")
	}
	// Crash recovery: nobody releases; after the lease every permit disappears on its own.
	advance(now, 11*time.Second)
	for _, h := range []string{"x", "y", "z"} {
		if !mustAcq(h, 3) {
			t.Fatalf("expired leases of crashed holders still block %s", h)
		}
	}
	if n, _ := s.Held(ctx, key); n != 3 {
		t.Fatalf("expired permits were not pruned: held=%d", n)
	}
	if _, err := s.Acquire(ctx, key, 0, time.Second, "h"); err == nil {
		t.Fatal("invalid limit accepted")
	}
}

func TestPermitsNeverExceedTheLimitUnderContention(t *testing.T) {
	c := newClient(t)
	var b6 [6]byte
	_, _ = rand.Read(b6[:])
	s := NewStore(c, "mailx:limittest:pc"+hex.EncodeToString(b6[:]))
	other := NewStore(newClient(t), s.ns) // a second process
	key := s.TenantPermitKey("busy")
	t.Cleanup(func() { c.Del(context.Background(), key) })
	var cur, peak, granted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := []*Store{s, other}[i%2]
			holder := "h" + string(rune('A'+i%26)) + hex.EncodeToString([]byte{byte(i)})
			ok, err := st.Acquire(context.Background(), key, 5, time.Minute, holder)
			if err != nil || !ok {
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			granted.Add(1)
			time.Sleep(5 * time.Millisecond)
			cur.Add(-1)
			_ = st.Release(context.Background(), key, holder)
		}()
	}
	wg.Wait()
	if peak.Load() > 5 {
		t.Fatalf("peak concurrent permits %d exceeded the limit 5", peak.Load())
	}
	if granted.Load() == 0 {
		t.Fatal("nothing was granted")
	}
	time.Sleep(50 * time.Millisecond)
	if n, _ := s.Held(context.Background(), key); n != 0 {
		t.Fatalf("permits leaked: %d", n)
	}
}

func TestPermitKeyExpiresWhenAbandoned(t *testing.T) {
	c := newClient(t)
	var b6 [6]byte
	_, _ = rand.Read(b6[:])
	s := NewStore(c, "mailx:limittest:pe"+hex.EncodeToString(b6[:]))
	key := s.DestinationPermitKey("example.com")
	if ok, err := s.Acquire(context.Background(), key, 2, 300*time.Millisecond, "crashed-worker"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ttl, _ := c.PTTL(context.Background(), key).Result(); ttl <= 0 || ttl > time.Second {
		t.Fatalf("a permit key must carry a TTL: %v", ttl)
	}
	if !waitGone(c, key, 8*time.Second) {
		t.Fatal("an abandoned destination key was not cleaned up")
	}
}

// waitGone polls until the key has expired. A bounded poll instead of a fixed sleep
// because Redis's clock inside a Docker/VM host can lag the host clock by hundreds of
// milliseconds (observed: plain SET .. PX 300 survived a 450 ms host sleep); the
// property under test is "expires on its own", not "expires within X host ms".
func waitGone(c *redis.Client, key string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if n, _ := c.Exists(context.Background(), key).Result(); n == 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestKeysUseOpaqueIdentifiersAndTheTenantHashTag(t *testing.T) {
	s := NewStore(nil, "")
	for _, k := range []string{s.TenantRequestKey("T"), s.APIKeyGuardKey("T", "K"), s.TenantRecipientKey("T")} {
		if k[:len(DefaultNamespace)] != DefaultNamespace {
			t.Fatal(k)
		}
	}
	// The three rate buckets of one tenant share a hash tag, so the multi-key script is valid on Redis Cluster.
	for _, k := range []string{s.TenantRequestKey("T"), s.APIKeyGuardKey("T", "K"), s.TenantRecipientKey("T")} {
		if want := "{T}"; !contains(k, want) {
			t.Fatalf("%s lacks the tenant hash tag", k)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestPolicyValidation(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("the defaults must be valid: %v", err)
	}
	mut := func(f func(*Policy)) Policy { p := DefaultPolicy(); f(&p); return p }
	for name, p := range map[string]Policy{
		"zero request rate":          mut(func(p *Policy) { p.TenantRequestRate = 0 }),
		"negative rate":              mut(func(p *Policy) { p.TenantRecipientRate = -1 }),
		"NaN rate":                   mut(func(p *Policy) { p.KeyRequestRate = math.NaN() }),
		"infinite rate":              mut(func(p *Policy) { p.TenantRequestRate = math.Inf(1) }),
		"overflowing rate":           mut(func(p *Policy) { p.TenantRequestRate = 1e12 }),
		"zero burst":                 mut(func(p *Policy) { p.TenantRequestBurst = 0 }),
		"overflowing burst":          mut(func(p *Policy) { p.TenantRecipientBurst = 1 << 40 }),
		"key limit above the tenant": mut(func(p *Policy) { p.KeyRequestRate = 1000 }),
		"key burst above the tenant": mut(func(p *Policy) { p.KeyRequestBurst = 10_000 }),
		"burst below max recipients": mut(func(p *Policy) { p.TenantRecipientBurst = 10 }),
		"zero concurrency":           mut(func(p *Policy) { p.TenantDeliveryConcurrency = 0 }),
		"absurd concurrency":         mut(func(p *Policy) { p.DestinationDeliveryConcurrency = 1 << 30 }),
		"zero queue cap":             mut(func(p *Policy) { p.TenantMaxQueuedMessages = 0 }),
		"tiny permit ttl":            mut(func(p *Policy) { p.PermitTTL = time.Second }),
		"huge permit ttl":            mut(func(p *Policy) { p.PermitTTL = 1000 * time.Hour }),
		"negative jitter":            mut(func(p *Policy) { p.RetryJitterPercent = -1 }),
		"excessive jitter":           mut(func(p *Policy) { p.RetryJitterPercent = 90 }),
		"zero recipients":            mut(func(p *Policy) { p.MaxRecipientsPerMessage = 0 }),
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for in, want := range map[time.Duration]int{-time.Second: 1, 0: 1, time.Nanosecond: 1, time.Second: 1, time.Second + 1: 2, 1500 * time.Millisecond: 2,
		59*time.Second + time.Millisecond: 60, 2 * time.Hour: 3600} {
		if got := RetryAfterSeconds(in); got != want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", in, got, want)
		}
	}
}

func FuzzPolicyValidateAndRetryAfter(f *testing.F) {
	f.Add(50.0, 100, 25.0, 50, 20.0, 500, 16, int64(600), 10, int64(1500))
	f.Fuzz(func(t *testing.T, rate float64, burst int, krate float64, kburst int, rrate float64, rburst, conc int, ttlSec int64, jitter int, wait int64) {
		p := Policy{TenantRequestRate: rate, TenantRequestBurst: burst, KeyRequestRate: krate, KeyRequestBurst: kburst,
			TenantRecipientRate: rrate, TenantRecipientBurst: rburst, TenantMaxQueuedMessages: conc, MaxPendingDispatch: conc,
			TenantDeliveryConcurrency: conc, DestinationDeliveryConcurrency: conc, PermitTTL: time.Duration(ttlSec) * time.Second,
			RetryJitterPercent: jitter, MaxRecipientsPerMessage: 50}
		_ = p.Validate() // must never panic
		if s := RetryAfterSeconds(time.Duration(wait)); s < 1 || s > 3600 {
			t.Fatalf("Retry-After %d out of bounds for %d", s, wait)
		}
	})
}

// BenchmarkAllowTwoBuckets measures the hot API decision: one script call, two
// buckets, real Redis. Run with -bench to reproduce the numbers in the design notes.
func BenchmarkAllowTwoBuckets(b *testing.B) {
	c := newClient(b)
	s := NewStore(c, "mailx:limitbench")
	b.Cleanup(func() { c.Del(context.Background(), s.TenantRequestKey("bench"), s.APIKeyGuardKey("bench", "k")) })
	tn := Bucket{Key: s.TenantRequestKey("bench"), Rate: 1e6, Burst: 1_000_000, Cost: 1}
	kb := Bucket{Key: s.APIKeyGuardKey("bench", "k"), Rate: 1e6, Burst: 1_000_000, Cost: 1}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := s.Allow(context.Background(), tn, kb); err != nil {
				b.Fatal(err)
			}
		}
	})
}
