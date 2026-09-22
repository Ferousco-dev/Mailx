package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/ratelimit"
	"github.com/Ferousco-dev/mailx/internal/storage"
	"github.com/redis/go-redis/v9"
)

// abuseRig is a full API stack with real PostgreSQL and real Redis, a frozen
// limiter clock the test advances explicitly, and helpers for tenants and keys.
type abuseRig struct {
	t     *testing.T
	db    *database.DB
	mux   http.Handler
	auth  *auth.Service
	limit *failableLimiter
	now   *atomic.Int64
	// domains maps tenant id -> its verified sending domain (a domain can be
	// verified by only one tenant at a time).
	domains map[string]string
}

// acctHandler is an authenticated handler that remembers its tenant's From address.
type acctHandler struct {
	http.Handler
	from string
}

// failableLimiter wraps the real Store so a test can simulate Redis being down.
type failableLimiter struct {
	*ratelimit.Store
	down atomic.Bool
}

func (f *failableLimiter) Allow(ctx context.Context, b ...ratelimit.Bucket) (ratelimit.Decision, error) {
	if f.down.Load() {
		return ratelimit.Decision{}, errors.New("redis: connection refused")
	}
	return f.Store.Allow(ctx, b...)
}

func testPolicy() ratelimit.Policy {
	p := ratelimit.DefaultPolicy()
	p.TenantRequestRate, p.TenantRequestBurst = 1, 1000
	p.KeyRequestRate, p.KeyRequestBurst = 1, 1000
	p.TenantRecipientRate, p.TenantRecipientBurst = 1, 1000
	p.MaxRecipientsPerMessage = 50
	return p
}

func newAbuseRig(t *testing.T, p ratelimit.Policy) *abuseRig {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis is required for abuse-control tests (they fail rather than skip): %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	ns := "mailx:apitest:" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		ctx := context.Background()
		it := rdb.Scan(ctx, 0, ns+"*", 1000).Iterator()
		for it.Next(ctx) {
			rdb.Del(ctx, it.Val())
		}
		_ = rdb.Close()
	})
	var now atomic.Int64
	now.Store(1_800_000_000_000_000)
	lim := &failableLimiter{Store: ratelimit.NewStore(rdb, ns).WithClock(now.Load)}

	db := newTestDB(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authSvc := auth.NewService(db, nil)
	eh := newEmailHandler(db, store)
	ac := &AbuseControls{Limiter: lim, Policy: p}
	eh.abuse = ac
	mux := newMux(eh, authSvc, func() error { return nil }, routeServices{abuse: ac})
	return &abuseRig{t: t, db: db, mux: mux, auth: authSvc, limit: lim, now: &now, domains: map[string]string{}}
}

func (r *abuseRig) advance(d time.Duration) { r.now.Add(d.Microseconds()) }

var abuseScopes = []string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead), string(auth.ScopeSuppressionsWrite), string(auth.ScopeSuppressionsRead)}

// tenant creates a tenant with a verified example.com and returns an actor for it.
func (r *abuseRig) tenant(name string) (database.Tenant, http.Handler) {
	r.t.Helper()
	tn, err := r.db.CreateTenant(context.Background(), name)
	if err != nil {
		r.t.Fatal(err)
	}
	dom := fmt.Sprintf("%s-%s.example.com", name, tn.ID[:6])
	verifyTestDomain(r.t, r.db, tn.ID, dom)
	r.domains[tn.ID] = dom
	return tn, r.keyFor(tn.ID)
}

func (r *abuseRig) keyFor(tenantID string) http.Handler {
	r.t.Helper()
	gen, _, err := r.auth.Create(context.Background(), tenantID, "k", abuseScopes, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return acctHandler{Handler: authInjector{next: r.mux, token: gen.Raw}, from: "a@" + r.domains[tenantID]}
}

func rcpts(n int, tag string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d@dest.test", tag, i)
	}
	return out
}

func sendTo(t *testing.T, h http.Handler, key string, to []string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"from": h.(acctHandler).from, "to": to, "subject": "hi", "text": "hello"}
	return doJSONWithKey(t, h, "POST", "/v1/emails", key, body)
}

func retryAfter(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	n, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || n < 1 {
		t.Fatalf("missing or invalid Retry-After %q on %d %s", rec.Header().Get("Retry-After"), rec.Code, rec.Body.String())
	}
	return n
}

func messageCount(t *testing.T, db *database.DB, tenantID string) int {
	t.Helper()
	list, err := db.ListMessages(context.Background(), tenantID, nil, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// ------------------------------------------------------------ request limits

func TestRequestLimitBurstThenTooManyRequestsThenRefill(t *testing.T) {
	p := testPolicy()
	p.TenantRequestRate, p.TenantRequestBurst = 1, 3
	p.KeyRequestRate, p.KeyRequestBurst = 1, 3
	rig := newAbuseRig(t, p)
	_, h := rig.tenant("acme")

	for i := 0; i < 3; i++ {
		if rec := doJSON(t, h, "GET", "/v1/emails", nil); rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := doJSON(t, h, "GET", "/v1/emails", nil)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "tenant_rate_limited" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if ra := retryAfter(t, rec); ra != 1 {
		t.Fatalf("Retry-After = %d, want 1 (one token per second)", ra)
	}
	// Retrying after the advertised interval works.
	rig.advance(time.Second)
	if rec := doJSON(t, h, "GET", "/v1/emails", nil); rec.Code != 200 {
		t.Fatalf("after Retry-After: %d %s", rec.Code, rec.Body.String())
	}
}

// Extra API keys must not raise a tenant's limit, and one key cannot spend the whole
// tenant allowance.
func TestAPIKeysCannotBypassTenantLimit(t *testing.T) {
	p := testPolicy()
	p.TenantRequestRate, p.TenantRequestBurst = 1, 4
	p.KeyRequestRate, p.KeyRequestBurst = 1, 3
	rig := newAbuseRig(t, p)
	tn, k1 := rig.tenant("acme")
	k2 := rig.keyFor(tn.ID)

	for i := 0; i < 3; i++ {
		if rec := doJSON(t, k1, "GET", "/v1/emails", nil); rec.Code != 200 {
			t.Fatalf("k1 #%d: %d", i, rec.Code)
		}
	}
	rec := doJSON(t, k1, "GET", "/v1/emails", nil)
	if rec.Code != 429 || errCode(t, rec) != "api_key_rate_limited" {
		t.Fatalf("k1 4th: %d %s", rec.Code, rec.Body.String())
	}
	// The refused k1 request was not charged to the tenant: k2 still gets the 4th token.
	if rec := doJSON(t, k2, "GET", "/v1/emails", nil); rec.Code != 200 {
		t.Fatalf("k2 1st: %d %s", rec.Code, rec.Body.String())
	}
	// Tenant bucket is now empty for EVERY key.
	rec = doJSON(t, k2, "GET", "/v1/emails", nil)
	if rec.Code != 429 || errCode(t, rec) != "tenant_rate_limited" {
		t.Fatalf("k2 2nd: %d %s", rec.Code, rec.Body.String())
	}
	k3 := rig.keyFor(tn.ID) // a brand-new key gains nothing
	if rec := doJSON(t, k3, "GET", "/v1/emails", nil); rec.Code != 429 {
		t.Fatalf("a new key must not bypass the tenant limit: %d", rec.Code)
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	p := testPolicy()
	p.TenantRequestRate, p.TenantRequestBurst = 1, 2
	p.KeyRequestRate, p.KeyRequestBurst = 1, 2
	rig := newAbuseRig(t, p)
	_, a := rig.tenant("a")
	_, b := rig.tenant("b")
	for i := 0; i < 5; i++ {
		doJSON(t, a, "GET", "/v1/emails", nil)
	}
	if rec := doJSON(t, a, "GET", "/v1/emails", nil); rec.Code != 429 {
		t.Fatalf("tenant a should be limited: %d", rec.Code)
	}
	if rec := doJSON(t, b, "GET", "/v1/emails", nil); rec.Code != 200 {
		t.Fatalf("tenant b must be unaffected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnauthenticatedRequestsNeverSpendTenantBuckets(t *testing.T) {
	p := testPolicy()
	p.TenantRequestRate, p.TenantRequestBurst = 1, 2
	p.KeyRequestRate, p.KeyRequestBurst = 1, 2
	rig := newAbuseRig(t, p)
	_, h := rig.tenant("acme")
	for i := 0; i < 20; i++ {
		rec := doRaw(t, rig.mux, "GET", "/v1/emails", "", nil)
		if rec.Code != 401 || rec.Header().Get("Retry-After") != "" {
			t.Fatalf("unauthenticated: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	for i := 0; i < 2; i++ {
		if rec := doJSON(t, h, "GET", "/v1/emails", nil); rec.Code != 200 {
			t.Fatalf("legit request %d after garbage: %d", i, rec.Code)
		}
	}
}

func TestHealthAndDocsAreNotRateLimited(t *testing.T) {
	p := testPolicy()
	p.TenantRequestBurst, p.KeyRequestBurst = 1, 1
	rig := newAbuseRig(t, p)
	for i := 0; i < 10; i++ {
		for _, path := range []string{"/health/live", "/health/ready", "/openapi.json"} {
			if rec := doRaw(t, rig.mux, "GET", path, "", nil); rec.Code != 200 {
				t.Fatalf("%s: %d", path, rec.Code)
			}
		}
	}
}

// Redis unavailable: writes fail closed with 503 + Retry-After, reads fail open.
func TestLimiterOutageFailsClosedForWritesOpenForReads(t *testing.T) {
	rig := newAbuseRig(t, testPolicy())
	tn, h := rig.tenant("acme")
	rig.limit.down.Store(true)

	rec := sendTo(t, h, "", rcpts(1, "x"))
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "rate_limiter_unavailable" || retryAfter(t, rec) != unavailableRetryAfter {
		t.Fatalf("send during outage: %d %s", rec.Code, rec.Body.String())
	}
	if n := messageCount(t, rig.db, tn.ID); n != 0 {
		t.Fatalf("a refused send stored %d messages", n)
	}
	if rec := doJSON(t, h, "POST", "/v1/suppressions", map[string]any{"email": "z@dest.test"}); rec.Code != 503 {
		t.Fatalf("mutating non-send route must also fail closed: %d", rec.Code)
	}
	if rec := doJSON(t, h, "GET", "/v1/emails", nil); rec.Code != 200 {
		t.Fatalf("reads fail open: %d %s", rec.Code, rec.Body.String())
	}
	// Recovery needs no restart.
	rig.limit.down.Store(false)
	if rec := sendTo(t, h, "", rcpts(1, "x")); rec.Code != http.StatusAccepted {
		t.Fatalf("after recovery: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------- recipient limits

func TestRecipientLimitBoundariesAndCombinedRoles(t *testing.T) {
	p := testPolicy()
	p.MaxRecipientsPerMessage = 3
	p.TenantRecipientRate, p.TenantRecipientBurst = 1, 5
	rig := newAbuseRig(t, p)
	tn, h := rig.tenant("acme")

	if rec := sendTo(t, h, "", rcpts(1, "a")); rec.Code != 202 { // cost 1 -> 4 left
		t.Fatalf("1 recipient: %d %s", rec.Code, rec.Body.String())
	}
	// To + Cc + Bcc combined count as 3 (the per-message max) -> 1 left.
	body := map[string]any{"from": h.(acctHandler).from, "to": []string{"t@dest.test"}, "cc": []string{"c@dest.test"}, "bcc": []string{"b@dest.test"}, "subject": "s", "text": "t"}
	if rec := doJSON(t, h, "POST", "/v1/emails", body); rec.Code != 202 {
		t.Fatalf("max recipients: %d %s", rec.Code, rec.Body.String())
	}
	// max+1 is a validation error, not a rate limit.
	if rec := sendTo(t, h, "", rcpts(4, "m")); rec.Code != 422 || errCode(t, rec) != "too_many_recipients" {
		t.Fatalf("max+1: %d %s", rec.Code, rec.Body.String())
	}
	// Two more deliverable recipients do not fit in the 1 remaining token.
	before := messageCount(t, rig.db, tn.ID)
	rec := sendTo(t, h, "", rcpts(2, "n"))
	if rec.Code != 429 || errCode(t, rec) != "recipient_rate_limited" {
		t.Fatalf("over recipient budget: %d %s", rec.Code, rec.Body.String())
	}
	retryAfter(t, rec)
	if messageCount(t, rig.db, tn.ID) != before {
		t.Fatal("a rate-limited send must not be stored")
	}
	// One recipient still fits (all-or-nothing did not spend the remaining token).
	if rec := sendTo(t, h, "", rcpts(1, "o")); rec.Code != 202 {
		t.Fatalf("remaining token was lost by the refused request: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSuppressedRecipientsAreNotCharged(t *testing.T) {
	p := testPolicy()
	p.MaxRecipientsPerMessage = 3
	p.TenantRecipientRate, p.TenantRecipientBurst = 1, 3
	rig := newAbuseRig(t, p)
	tn, h := rig.tenant("acme")
	for _, e := range []string{"s0@dest.test", "s1@dest.test"} {
		if _, _, err := rig.db.CreateSuppression(context.Background(), database.NewSuppression{TenantID: tn.ID, Email: e, Reason: "manual", Source: "api"}); err != nil {
			t.Fatal(err)
		}
	}
	// 3 recipients, 2 suppressed => cost 1. Three such sends fit in burst 3.
	to := []string{"s0@dest.test", "s1@dest.test", "live@dest.test"}
	for i := 0; i < 3; i++ {
		if rec := sendTo(t, h, "", to); rec.Code != 202 {
			t.Fatalf("send %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := sendTo(t, h, "", to); rec.Code != 429 {
		t.Fatalf("4th send should exceed the deliverable budget: %d", rec.Code)
	}
	// All suppressed is still the v0.30 422 and costs nothing.
	if rec := sendTo(t, h, "", []string{"s0@dest.test"}); rec.Code != 422 || errCode(t, rec) != "all_recipients_suppressed" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------- idempotency

func TestIdempotentReplayIsNotChargedAndConflictStillWins(t *testing.T) {
	p := testPolicy()
	p.TenantRecipientRate, p.TenantRecipientBurst = 1, 3
	rig := newAbuseRig(t, p)
	tn, h := rig.tenant("acme")

	first := sendTo(t, h, "key-1", rcpts(1, "a"))
	if first.Code != 202 {
		t.Fatalf("%d %s", first.Code, first.Body.String())
	}
	id := decodeEmail(t, first).ID
	for i := 0; i < 10; i++ { // replays cost nothing
		rec := sendTo(t, h, "key-1", rcpts(1, "a"))
		if rec.Code != 202 || rec.Header().Get("Idempotency-Replayed") != "true" || decodeEmail(t, rec).ID != id {
			t.Fatalf("replay %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	// 2 tokens remain: two fresh sends fit, the third does not.
	for i := 0; i < 2; i++ {
		if rec := sendTo(t, h, "", rcpts(1, fmt.Sprintf("f%d", i))); rec.Code != 202 {
			t.Fatalf("fresh %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := sendTo(t, h, "", rcpts(1, "over")); rec.Code != 429 {
		t.Fatalf("over budget: %d", rec.Code)
	}
	// Even with the bucket empty, the same key + a DIFFERENT payload is a 409 conflict, not a 429.
	rec := sendTo(t, h, "key-1", rcpts(2, "different"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("same key, different payload: %d %s", rec.Code, rec.Body.String())
	}
	// And the same key + same payload still replays with an empty bucket.
	if rec := sendTo(t, h, "key-1", rcpts(1, "a")); rec.Code != 202 || rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay with empty bucket: %d %s", rec.Code, rec.Body.String())
	}
	if n := messageCount(t, rig.db, tn.ID); n != 3 {
		t.Fatalf("%d messages, want 3", n)
	}
}

// A request refused after claiming its key must give the claim back so the client
// can retry the SAME key after Retry-After.
func TestRefusedSendReleasesItsIdempotencyClaim(t *testing.T) {
	p := testPolicy()
	p.TenantRecipientRate, p.TenantRecipientBurst = 1, 1
	rig := newAbuseRig(t, p)
	_, h := rig.tenant("acme")
	if rec := sendTo(t, h, "", rcpts(1, "a")); rec.Code != 202 {
		t.Fatal(rec.Body.String())
	}
	rec := sendTo(t, h, "retry-me", rcpts(1, "b"))
	if rec.Code != 429 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	wait := retryAfter(t, rec)
	rig.advance(time.Duration(wait) * time.Second)
	rec = sendTo(t, h, "retry-me", rcpts(1, "b"))
	if rec.Code != 202 || rec.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatalf("the refused key must be reusable, got %d %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------- queue cap & backpressure

func TestTenantQueueCapIsPerTenantAndReleasesTheClaim(t *testing.T) {
	p := testPolicy()
	p.TenantMaxQueuedMessages = 2
	rig := newAbuseRig(t, p)
	a, ha := rig.tenant("a")
	_, hb := rig.tenant("b")
	for i := 0; i < 2; i++ {
		if rec := sendTo(t, ha, "", rcpts(1, fmt.Sprintf("q%d", i))); rec.Code != 202 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	rec := sendTo(t, ha, "cap-key", rcpts(1, "z"))
	if rec.Code != 429 || errCode(t, rec) != "tenant_queue_full" || retryAfter(t, rec) != capacityRetryAfter {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := messageCount(t, rig.db, a.ID); n != 2 {
		t.Fatalf("%d", n)
	}
	// Another tenant is unaffected by tenant a's backlog.
	if rec := sendTo(t, hb, "", rcpts(1, "b")); rec.Code != 202 {
		t.Fatalf("tenant b: %d %s", rec.Code, rec.Body.String())
	}
	// Draining frees capacity; the refused key was released and now works.
	msgs, _ := rig.db.ListMessages(context.Background(), a.ID, nil, 10, nil)
	now := time.Now()
	if err := rig.db.UpdateMessageStatus(context.Background(), msgs[0].ID, database.StatusDelivered, &now); err != nil {
		t.Fatal(err)
	}
	if rec := sendTo(t, ha, "cap-key", rcpts(1, "z")); rec.Code != 202 {
		t.Fatalf("after drain: %d %s", rec.Code, rec.Body.String())
	}
}

func TestGlobalBackpressureIs503NotTenantBlame(t *testing.T) {
	p := testPolicy()
	p.MaxPendingDispatch = 2
	rig := newAbuseRig(t, p)
	_, ha := rig.tenant("a")
	_, hb := rig.tenant("b")
	sendTo(t, ha, "", rcpts(1, "a1"))
	sendTo(t, hb, "", rcpts(1, "b1"))
	rec := sendTo(t, ha, "", rcpts(1, "a2"))
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "system_busy" || retryAfter(t, rec) != capacityRetryAfter {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// It is system-wide: a quiet third party is also asked to retry (503, not 429).
	if rec := sendTo(t, hb, "", rcpts(1, "b2")); rec.Code != 503 {
		t.Fatalf("%d", rec.Code)
	}
	// Reads are not affected by send backpressure.
	if rec := doJSON(t, ha, "GET", "/v1/emails", nil); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

// ---------------------------------------------------------------- concurrency

func TestConcurrentSendsNeverExceedTheRecipientBudget(t *testing.T) {
	p := testPolicy()
	p.TenantRecipientRate, p.TenantRecipientBurst = 1, 10
	rig := newAbuseRig(t, p)
	tn, _ := rig.tenant("acme")
	keys := []http.Handler{rig.keyFor(tn.ID), rig.keyFor(tn.ID), rig.keyFor(tn.ID)}

	var ok, limited, other atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := sendTo(t, keys[i%3], "", rcpts(1, fmt.Sprintf("c%d", i)))
			switch rec.Code {
			case 202:
				ok.Add(1)
			case 429:
				limited.Add(1)
			default:
				other.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 10 || limited.Load() != 30 || other.Load() != 0 {
		t.Fatalf("accepted=%d limited=%d other=%d, want exactly 10/30/0", ok.Load(), limited.Load(), other.Load())
	}
	if n := messageCount(t, rig.db, tn.ID); n != 10 {
		t.Fatalf("%d stored", n)
	}
}

// --------------------------------------------------------------- privacy

func TestRefusalsLeakNothingAboutOtherTenants(t *testing.T) {
	p := testPolicy()
	p.TenantRecipientRate, p.TenantRecipientBurst = 1, 1
	rig := newAbuseRig(t, p)
	tn, h := rig.tenant("acme")
	sendTo(t, h, "", rcpts(1, "a"))
	rec := sendTo(t, h, "", rcpts(1, "b"))
	body := rec.Body.String()
	for _, secret := range []string{tn.ID, "b0@dest.test", "mailx:limit"} {
		if secret != "" && strings.Contains(body, secret) {
			t.Fatalf("429 body leaks %q: %s", secret, body)
		}
	}
}

// Hot-path cost of the per-request limiter (one Redis round trip, two buckets) against the same handler with no limiter.
func benchRequestLimit(b *testing.B, withLimiter bool) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.Close()
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		b.Fatal(err)
	}
	var ac *AbuseControls
	if withLimiter {
		p := testPolicy()
		p.TenantRequestRate, p.TenantRequestBurst = 1_000_000, 1_000_000
		p.KeyRequestRate, p.KeyRequestBurst = 1_000_000, 1_000_000
		ac = &AbuseControls{Limiter: ratelimit.NewStore(rdb, "mailx:apibench"), Policy: p}
	}
	h := requestLimitMiddleware(ac)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	ctx := withAuth(withTenant(context.Background(), "tenant-bench"), []string{"emails:read"}, "key-bench")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/emails", nil).WithContext(ctx))
		}
	})
}

func BenchmarkRequestWithoutLimiter(b *testing.B) { benchRequestLimit(b, false) }
func BenchmarkRequestWithLimiter(b *testing.B)    { benchRequestLimit(b, true) }
