package observability

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Ferousco-dev/mailx/internal/buildinfo"
)

func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	m, err := NewMetrics(buildinfo.Info{Version: "v-test", Commit: "c-test"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func gather(t *testing.T, m *Metrics) map[string]bool {
	t.Helper()
	fams, err := m.Registry().Gather() // pedantic registry: fails on any inconsistency
	if err != nil {
		t.Fatalf("pedantic gather: %v", err)
	}
	names := map[string]bool{}
	for _, f := range fams {
		names[f.GetName()] = true
		for _, metric := range f.GetMetric() {
			if len(metric.GetLabel()) > 3 {
				t.Fatalf("%s has %d labels", f.GetName(), len(metric.GetLabel()))
			}
		}
		if strings.Contains(f.GetName(), "duration") && !strings.HasSuffix(f.GetName(), "_seconds") {
			t.Fatalf("duration metric %s is not in seconds", f.GetName())
		}
	}
	return names
}

func TestEveryMetricFamilyIsExposed(t *testing.T) {
	m := newTestMetrics(t)
	m.SetQueueDepth(func(context.Context) (int64, error) { return 3, nil })
	m.HTTPRequest("GET", "/v1/emails", 200, time.Millisecond)
	m.SMTPSessionStarted()
	m.SMTPSessionEnded("completed")
	m.SMTPSessionRejected()
	m.SMTPMessage("accepted")
	m.DeliveryAttempt("accepted", "terminal_success", time.Second)
	m.QueueOp("claim", nil)
	m.WebhookAttempt("succeeded", time.Millisecond)
	m.TLSResult("opportunistic", "established", "1.3")
	m.AuthResult("plain", "success")
	m.SignResult("rsa-sha256", "signed")
	m.SPFResult("direct", "verified")
	names := gather(t, m)
	for _, want := range []string{
		"mailx_build_info", "mailx_http_requests_total", "mailx_http_request_duration_seconds",
		"mailx_smtp_sessions_total", "mailx_smtp_active_sessions", "mailx_smtp_messages_total",
		"mailx_delivery_attempts_total", "mailx_delivery_attempt_duration_seconds",
		"mailx_queue_operations_total", "mailx_queue_depth", "mailx_webhook_attempts_total",
		"mailx_webhook_attempt_duration_seconds", "mailx_smtp_tls_sessions_total", "mailx_smtp_auth_attempts_total", "mailx_dkim_signatures_total", "mailx_spf_verifications_total", "go_goroutines",
	} {
		if !names[want] {
			t.Errorf("family %s missing", want)
		}
	}
	for name := range names {
		if strings.HasSuffix(name, "_total") == false && strings.Contains(name, "attempts") && !strings.Contains(name, "duration") {
			t.Errorf("counter %s lacks _total", name)
		}
	}
}

func TestLabelValuesAreBounded(t *testing.T) {
	m := newTestMetrics(t)
	m.HTTPRequest("BREW", "/v1/emails", 799, 0)
	m.SMTPSessionStarted()
	m.SMTPSessionEnded("user@example.com")
	m.SMTPMessage("secret-body")
	m.DeliveryAttempt("550 mailbox unknown for bob@example.com", "weird", 0)
	m.QueueOp("delete_everything", errors.New("boom for bob@example.com"))
	m.WebhookAttempt("https://evil.example/hook", 0)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, banned := range []string{"BREW", "user@example.com", "secret-body", "550 mailbox", "bob@example", "delete_everything", "evil.example", `status_class="7xx"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("unbounded label value %q reached exposition", banned)
		}
	}
	if !strings.Contains(body, `method="other"`) || !strings.Contains(body, `kind="other"`) || !strings.Contains(body, `status_class="other"`) {
		t.Fatal("unexpected values must collapse to \"other\"")
	}
}

func TestMetricsHandlerContentTypeAndBuildInfo(t *testing.T) {
	m := newTestMetrics(t)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("code=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `mailx_build_info{commit="c-test",version="v-test"} 1`) {
		t.Fatal("build info missing from exposition")
	}
}

func TestQueueDepthCollectionFailureCountsErrorAndEmitsNoSample(t *testing.T) {
	m := newTestMetrics(t)
	m.SetQueueDepth(func(context.Context) (int64, error) { return 0, errors.New("redis down") })
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if strings.Contains(body, "mailx_queue_depth ") || !strings.Contains(body, "mailx_queue_depth_errors_total 1") {
		t.Fatalf("depth failure handling wrong:\n%s", body)
	}
	if strings.Contains(body, "redis down") {
		t.Fatal("error text leaked into metrics")
	}
	m.SetQueueDepth(func(context.Context) (int64, error) { panic("depth panic") })
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("panicking depth source broke scrape: %d", rec.Code)
	}
}

func TestNilMetricsIsInert(t *testing.T) {
	var m *Metrics
	m.HTTPRequest("GET", "/x", 200, 0)
	m.SMTPSessionStarted()
	m.SMTPSessionEnded("completed")
	m.SMTPSessionRejected()
	m.SMTPMessage("accepted")
	m.DeliveryAttempt("accepted", "retry", 0)
	m.QueueOp("ack", nil)
	m.WebhookAttempt("failed", 0)
	m.SetQueueDepth(nil)
}

func TestMetricsRecoverFromInternalPanic(t *testing.T) {
	m := newTestMetrics(t)
	m.httpTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wrong"}, []string{"only_one"})
	m.HTTPRequest("GET", "/x", 200, 0) // label-count mismatch panics inside; must be swallowed
}

func TestOperatorMuxIsolationAndMetricsEndpoint(t *testing.T) {
	m := newTestMetrics(t)
	srv := httptest.NewServer(OperatorMux(m, NewReadiness(nil)))
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/metrics"); code != 200 {
		t.Fatalf("/metrics = %d", code)
	}
	for _, p := range []string{"/v1/emails", "/v1/webhooks", "/openapi.json", "/docs"} {
		if code, _ := get(p); code != 404 {
			t.Fatalf("operator listener served %s (%d)", p, code)
		}
	}
}

func scrape(m *Metrics) string {
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestTLSMetricLabelsAreBounded(t *testing.T) {
	m := newTestMetrics(t)
	m.TLSResult("opportunistic", "established", "1.3")
	m.TLSResult("required", "verify_failed", "none")
	// Hostile values (host names, cert subjects, raw errors) must collapse.
	m.TLSResult("mx.victim.example", "x509: certificate is valid for evil.example", "TLS 9.9")
	out := scrape(m)
	for _, want := range []string{
		`mailx_smtp_tls_sessions_total{outcome="established",policy="opportunistic",version="1.3"} 1`,
		`mailx_smtp_tls_sessions_total{outcome="verify_failed",policy="required",version="none"} 1`,
		`mailx_smtp_tls_sessions_total{outcome="other",policy="other",version="other"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, banned := range []string{"victim", "evil.example", "x509", "TLS 9.9"} {
		if strings.Contains(out, banned) {
			t.Fatalf("unbounded TLS label value %q reached exposition", banned)
		}
	}
	gather(t, m)
	var nilMetrics *Metrics
	nilMetrics.TLSResult("required", "established", "1.2") // inert, no panic
}

func TestAuthMetricLabelsAreBounded(t *testing.T) {
	m := newTestMetrics(t)
	m.AuthResult("plain", "success")
	m.AuthResult("none", "not_advertised")
	// Hostile values (user names, hosts, server text) must collapse.
	m.AuthResult("user@example.com", "535 5.7.8 bad password for alice")
	out := scrape(m)
	for _, want := range []string{
		`mailx_smtp_auth_attempts_total{mechanism="plain",outcome="success"} 1`,
		`mailx_smtp_auth_attempts_total{mechanism="none",outcome="not_advertised"} 1`,
		`mailx_smtp_auth_attempts_total{mechanism="other",outcome="other"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, banned := range []string{"alice", "user@example.com", "535"} {
		if strings.Contains(out, banned) {
			t.Fatalf("unbounded AUTH label value %q reached exposition", banned)
		}
	}
	gather(t, m)
	var nilMetrics *Metrics
	nilMetrics.AuthResult("plain", "success")
}

func TestDKIMMetricLabelsAreBounded(t *testing.T) {
	m := newTestMetrics(t)
	m.SignResult("rsa-sha256", "signed")
	m.SignResult("rsa-sha256", "key_decrypt_failed")
	// Hostile values (domains, selectors, key data, raw errors) must collapse.
	m.SignResult("example.com", "mx20260921ab12 key BEGIN PRIVATE KEY")
	out := scrape(m)
	for _, want := range []string{
		`mailx_dkim_signatures_total{algorithm="rsa-sha256",outcome="signed"} 1`,
		`mailx_dkim_signatures_total{algorithm="rsa-sha256",outcome="key_decrypt_failed"} 1`,
		`mailx_dkim_signatures_total{algorithm="other",outcome="other"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, banned := range []string{"example.com", "mx2026", "PRIVATE"} {
		if strings.Contains(out, banned) {
			t.Fatalf("unbounded DKIM label value %q reached exposition", banned)
		}
	}
	gather(t, m)
	var nilMetrics *Metrics
	nilMetrics.SignResult("rsa-sha256", "signed")
}

func TestSPFMetricLabelsAreBounded(t *testing.T) {
	m := newTestMetrics(t)
	m.SPFResult("direct", "verified")
	m.SPFResult("relay", "temporary_error")
	// Hostile values (domains, addresses, raw records, DNS errors) must collapse.
	m.SPFResult("example.com", "v=spf1 ip4:203.0.113.9 -all lookup failed on 10.0.0.1")
	out := scrape(m)
	for _, want := range []string{
		`mailx_spf_verifications_total{mode="direct",outcome="verified"} 1`,
		`mailx_spf_verifications_total{mode="relay",outcome="temporary_error"} 1`,
		`mailx_spf_verifications_total{mode="other",outcome="other"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, banned := range []string{"example.com", "203.0.113", "10.0.0.1", "v=spf1"} {
		if strings.Contains(out, banned) {
			t.Fatalf("unbounded SPF label value %q reached exposition", banned)
		}
	}
	var nilMetrics *Metrics
	nilMetrics.SPFResult("direct", "verified")
}
