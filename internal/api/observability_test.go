package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/buildinfo"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

const (
	markerBody  = "PRIVATE-BODY-MARKER"
	markerQuery = "PRIVATE-QUERY-MARKER"
)

type observedAPI struct {
	handler http.Handler
	logs    *lockedBuf
	metrics *observability.Metrics
	rawKey  string
	tenant  database.Tenant
}

// newObservedAPI builds the real middleware chain (request ID, recover, body
// limit) around the real mux and authentication.
func newObservedAPI(t *testing.T, readiness func() error) observedAPI {
	t.Helper()
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authSvc := auth.NewService(db, nil)
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "obs key",
		[]string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	logger, _ := observability.NewLogger(logs, "debug", "json")
	metrics, err := observability.NewMetrics(buildinfo.Info{Version: "t", Commit: "t"})
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(newEmailHandler(db, store), authSvc, readiness)
	h := chain(mux, withRequestIDMiddleware(logger, metrics), withRecoverMiddleware(logger), limitBody)
	return observedAPI{h, logs, metrics, gen.Raw, tenant}
}

func (o observedAPI) do(method, target, body string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if auth {
		req.Header.Set("Authorization", "Bearer "+o.rawKey)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	o.handler.ServeHTTP(rec, req)
	return rec
}

func (o observedAPI) lastAccessLog(t *testing.T) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(o.logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "http_request" {
			last = rec
		}
	}
	if last == nil {
		t.Fatalf("no http_request record in:\n%s", o.logs.String())
	}
	return last
}

func TestAccessLogCorrelationTenantRouteAndPrivacy(t *testing.T) {
	o := newObservedAPI(t, func() error { return nil })
	body := `{"from":"a@example.com","to":["b@example.com"],"subject":"` + markerBody + `","text":"` + markerBody + `"}`
	rec := o.do("POST", "/v1/emails?q="+markerQuery, body, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send failed: %d %s", rec.Code, rec.Body.String())
	}
	id := rec.Header().Get("X-Request-Id")
	line := o.lastAccessLog(t)
	if line["request_id"] != id || !strings.HasPrefix(id, "req_") || line["method"] != "POST" ||
		line["route"] != "/v1/emails" || line["status"] != float64(202) || line["tenant_id"] != o.tenant.ID {
		t.Fatalf("access log = %v (request id %q)", line, id)
	}
	if _, ok := line["duration_ms"]; !ok {
		t.Fatal("duration_ms missing")
	}
	out := o.logs.String()
	for _, banned := range []string{markerBody, markerQuery, o.rawKey, "Authorization", "Bearer", "a@example.com", "b@example.com"} {
		if strings.Contains(out, banned) {
			t.Fatalf("log leaked %q:\n%s", banned, out)
		}
	}
}

func TestAccessLogRoutePatternForPathParamsAndUnmatched(t *testing.T) {
	o := newObservedAPI(t, func() error { return nil })
	o.do("GET", "/v1/emails/some-unique-id-123", "", true)
	if got := o.lastAccessLog(t)["route"]; got != "/v1/emails/{id}" {
		t.Fatalf("route = %v", got)
	}
	o.do("GET", "/no/such/path/"+markerQuery, "", false)
	line := o.lastAccessLog(t)
	if line["route"] != "unmatched" || strings.Contains(o.logs.String(), markerQuery) {
		t.Fatalf("unmatched request must not log its path: %v", line)
	}
}

func TestAccessLogAuthFailureHasNoTenantAndClientRequestIDIsNotTrusted(t *testing.T) {
	o := newObservedAPI(t, func() error { return nil })
	req := httptest.NewRequest("GET", "/v1/emails", nil)
	req.Header.Set("X-Request-Id", "attacker-chosen")
	req.Header.Set("Authorization", "Bearer mx_bad_secretvalue")
	rec := httptest.NewRecorder()
	o.handler.ServeHTTP(rec, req)
	line := o.lastAccessLog(t)
	if rec.Code != 401 || line["status"] != float64(401) || line["route"] != "/v1/" {
		t.Fatalf("code=%d line=%v", rec.Code, line)
	}
	if _, has := line["tenant_id"]; has {
		t.Fatal("unauthenticated request must not log a tenant")
	}
	if line["request_id"] == "attacker-chosen" || strings.Contains(o.logs.String(), "secretvalue") || strings.Contains(o.logs.String(), "attacker-chosen") {
		t.Fatalf("untrusted input reached logs:\n%s", o.logs.String())
	}
}

func TestPanicIsRecoveredCorrelatedAndAccessLogged(t *testing.T) {
	logs := &lockedBuf{}
	logger, _ := observability.NewLogger(logs, "debug", "json")
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("panic-value-" + markerBody) })
	h := chain(boom, withRequestIDMiddleware(logger, nil), withRecoverMiddleware(logger), limitBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/anything", nil))
	id := rec.Header().Get("X-Request-Id")
	var body struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 500 || body.Error.RequestID != id {
		t.Fatalf("code=%d body=%s id=%s", rec.Code, rec.Body.String(), id)
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"http_panic"`) || !strings.Contains(out, `"status":500`) || !strings.Contains(out, id) {
		t.Fatalf("panic not attributable:\n%s", out)
	}
	if strings.Contains(out, markerBody) {
		t.Fatalf("panic value leaked:\n%s", out)
	}
}

func TestHTTPMetricsUseBoundedLabels(t *testing.T) {
	o := newObservedAPI(t, func() error { return nil })
	o.do("GET", "/v1/emails", "", true)
	o.do("GET", "/v1/emails/abc", "", true)
	o.do("GET", "/zzz/"+markerQuery, "", false)
	rec := httptest.NewRecorder()
	o.metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`mailx_http_requests_total{method="GET",route="/v1/emails",status_class="2xx"} 1`,
		`mailx_http_requests_total{method="GET",route="/v1/emails/{id}",status_class="4xx"} 1`,
		`mailx_http_requests_total{method="GET",route="unmatched",status_class="4xx"} 1`,
		`mailx_http_request_duration_seconds_count{method="GET",route="/v1/emails"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, markerQuery) || strings.Contains(out, "abc") {
		t.Fatal("request-derived value reached metric labels")
	}
}

func TestHealthReadyReportsBoundedComponentsOnly(t *testing.T) {
	failing := func() error {
		return &observability.NotReadyError{Failed: []string{"postgres"}}
	}
	o := newObservedAPI(t, failing)
	rec := o.do("GET", "/health/ready", "", false)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"failed":["postgres"]`) || strings.Contains(rec.Body.String(), "reason") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := newObservedAPI(t, func() error { return errors.New("dial tcp 10.1.2.3:5432: password authentication failed") })
	rec = raw.do("GET", "/health/ready", "", false)
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "10.1.2.3") || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("raw error leaked: %d %s", rec.Code, rec.Body.String())
	}
	if live := raw.do("GET", "/health/live", "", false); live.Code != 200 {
		t.Fatalf("liveness = %d", live.Code)
	}
	ok := newObservedAPI(t, func() error { return nil })
	if rec := ok.do("GET", "/health/ready", "", false); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ready") {
		t.Fatalf("ready = %d %s", rec.Code, rec.Body.String())
	}
}

type panicLogWriter struct{}

func (panicLogWriter) Write([]byte) (int, error) { panic("log sink down") }

func TestBrokenLoggerAndNilMetricsDoNotChangeResponses(t *testing.T) {
	logger, _ := observability.NewLogger(panicLogWriter{}, "debug", "json")
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := chain(ok, withRequestIDMiddleware(logger, nil), withRecoverMiddleware(logger), limitBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusTeapot || rec.Header().Get("X-Request-Id") == "" {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestStatusWriterUnwrapsForResponseController(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: 200}
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Fatalf("Flush through wrapper: %v", err)
	}
	if sw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap must return the underlying writer")
	}
}
