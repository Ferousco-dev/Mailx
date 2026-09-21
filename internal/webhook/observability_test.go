package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/buildinfo"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/observability"
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestWebhookLogsCorrelateAndNeverExposeSecretsURLsOrResponses(t *testing.T) {
	db := webhookTestDB(t)
	ctx := context.Background()
	box, _ := NewSecretBox(make([]byte, 32))
	tenant, _ := db.CreateTenant(ctx, "obs-webhook")

	const respMarker = "REMOTE-RESPONSE-BODY-MARKER"
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, respMarker, http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	buf := &safeBuf{}
	logger, _ := observability.NewLogger(buf, "debug", "json")
	metrics, err := observability.NewMetrics(buildinfo.Info{Version: "t", Commit: "t"})
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(db, box, URLPolicy{AllowHTTP: true, AllowPrivate: true})
	service.WithLogger(logger)
	var secrets, urls []string
	var subIDs []string
	for _, srv := range []*httptest.Server{ok, bad} {
		created, err := service.Create(ctx, tenant.ID, srv.URL+"/hook-path-marker", []string{EventQueued})
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, created.Secret)
		urls = append(urls, srv.URL)
		subIDs = append(subIDs, created.Subscription.ID)
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	if _, err := db.InsertMessage(ctx, database.NewMessage{ID: hex.EncodeToString(raw), TenantID: tenant.ID, MailFrom: "s@example.com",
		Recipients: []database.RecipientInput{{Address: "r@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	if res, err := db.FanOutWebhookEvents(ctx, 10); err != nil || res.Deliveries != 2 {
		t.Fatalf("fan-out %+v %v", res, err)
	}

	pool, err := NewWorkerPool(db, box, NewClient(URLPolicy{AllowHTTP: true, AllowPrivate: true}),
		WorkerConfig{Workers: 1, ClaimLease: time.Minute, PollInterval: 10 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.WithObservability(logger, metrics)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = pool.Run(runCtx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(buf.String(), `"msg":"webhook_attempt"`) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("attempts not logged:\n%s", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if _, err := service.RotateSecret(ctx, tenant.ID, subIDs[0]); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, tenant.ID, subIDs[1]); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	for _, want := range []string{`"msg":"webhook_claimed"`, `"outcome":"succeeded"`, `"outcome":"retrying"`, `"response_code":503`,
		`"error_category":"http_retryable"`, `"next_retry_at"`, `"tenant_id":"` + tenant.ID + `"`, `"subscription_id":"` + subIDs[0] + `"`,
		`"delivery_id"`, `"event_id"`, `"attempt":1`, `"msg":"webhook_secret_rotated"`, `"msg":"webhook_subscription_disabled"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s", want)
		}
	}
	banned := append(append([]string{respMarker, "hook-path-marker", "MailX-Webhook-Signature", "v1=", "whsec_"}, secrets...), urls...)
	for _, b := range banned {
		if strings.Contains(out, b) {
			t.Fatalf("webhook log leaked %q:\n%s", b, out)
		}
	}
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{`mailx_webhook_attempts_total{outcome="succeeded"} 1`, `mailx_webhook_attempts_total{outcome="retrying"} 1`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestWebhookObservabilityDefaultsAreSilentAndSafe(t *testing.T) {
	var s Service
	s.logger().Info("noop")
	p := &WorkerPool{}
	p.WithObservability(nil, nil)
}
