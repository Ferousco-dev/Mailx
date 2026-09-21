package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ok(context.Context) error { return nil }

func fail(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432 password=hunter2") }

func block(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func ready(t *testing.T, r *Readiness, ctx context.Context) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/health/ready", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ReadyHandler().ServeHTTP(rec, req)
	if rec.Body.Len() >= 1024 {
		t.Fatalf("health body %d bytes", rec.Body.Len())
	}
	return rec.Code, rec.Body.String()
}

func TestReadinessSuccessAndComponentFailures(t *testing.T) {
	code, body := ready(t, NewReadiness(map[string]Check{"postgres": ok, "redis": ok}), context.Background())
	if code != 200 || !strings.Contains(body, `"ready"`) {
		t.Fatalf("code=%d body=%s", code, body)
	}
	code, body = ready(t, NewReadiness(map[string]Check{"postgres": fail, "redis": ok}), context.Background())
	if code != 503 || !strings.Contains(body, `"failed":["postgres"]`) {
		t.Fatalf("postgres failure: code=%d body=%s", code, body)
	}
	code, body = ready(t, NewReadiness(map[string]Check{"postgres": ok, "redis": fail}), context.Background())
	if code != 503 || !strings.Contains(body, `"failed":["redis"]`) {
		t.Fatalf("redis failure: code=%d body=%s", code, body)
	}
	for _, leak := range []string{"hunter2", "10.0.0.5", "dial tcp"} {
		if strings.Contains(body, leak) {
			t.Fatalf("raw dependency error leaked: %s", body)
		}
	}
}

func TestReadinessCancellationAndPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, body := ready(t, NewReadiness(map[string]Check{"postgres": block, "redis": block}), ctx)
	if code != 503 || !strings.Contains(body, "postgres") || !strings.Contains(body, "redis") {
		t.Fatalf("cancelled: code=%d body=%s", code, body)
	}
	code, _ = ready(t, NewReadiness(map[string]Check{"postgres": func(context.Context) error { panic("x") }, "redis": ok}), context.Background())
	if code != 503 {
		t.Fatalf("panicking check must report not ready, got %d", code)
	}
}

func TestReadinessTimeoutBoundedWhenChecksIgnoreContext(t *testing.T) {
	stuck := func(context.Context) error { time.Sleep(5 * time.Second); return nil }
	start := time.Now()
	code, body := ready(t, NewReadiness(map[string]Check{"postgres": stuck, "redis": block}), context.Background())
	if elapsed := time.Since(start); elapsed > 2200*time.Millisecond {
		t.Fatalf("readiness took %v (limit 2.2s)", elapsed)
	}
	if code != 503 || !strings.Contains(body, "postgres") || !strings.Contains(body, "redis") {
		t.Fatalf("code=%d body=%s", code, body)
	}
}

func TestLivenessNeverCallsDependencies(t *testing.T) {
	called := false
	r := NewReadiness(map[string]Check{"postgres": func(context.Context) error { called = true; return errors.New("down") }})
	mux := OperatorMux(newTestMetrics(t), r)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/health/live", nil))
	if rec.Code != http.StatusOK || called {
		t.Fatalf("liveness code=%d dependencyCalled=%v", rec.Code, called)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/health/ready", nil))
	if rec.Code != 503 {
		t.Fatalf("readiness must fail while liveness passes, got %d", rec.Code)
	}
}

func TestReadinessRecoversWhenDependencyRecovers(t *testing.T) {
	healthy := false
	r := NewReadiness(map[string]Check{"redis": func(context.Context) error {
		if !healthy {
			return errors.New("down")
		}
		return nil
	}})
	if code, _ := ready(t, r, context.Background()); code != 503 {
		t.Fatalf("expected 503, got %d", code)
	}
	healthy = true
	if code, _ := ready(t, r, context.Background()); code != 200 {
		t.Fatalf("expected recovery to 200, got %d", code)
	}
}

func TestCheckReturnsBoundedError(t *testing.T) {
	err := NewReadiness(map[string]Check{"redis": fail, "postgres": fail}).Check(context.Background())
	var nr *NotReadyError
	if !errors.As(err, &nr) || strings.Join(nr.Failed, ",") != "postgres,redis" || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v", err)
	}
}
