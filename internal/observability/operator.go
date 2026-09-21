package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ReadinessTimeout bounds the whole readiness check, however many
// dependencies it queries.
const ReadinessTimeout = 2 * time.Second

// Check probes one dependency. Its error is never exposed to callers.
type Check func(context.Context) error

// Readiness runs all dependency checks concurrently under one shared
// deadline. Component names are a fixed, bounded vocabulary chosen by the
// composition root (e.g. "postgres", "redis").
type Readiness struct {
	checks map[string]Check
}

func NewReadiness(checks map[string]Check) *Readiness { return &Readiness{checks: checks} }

// Failed returns the sorted names of components that failed, timed out, or
// panicked. Empty means ready. A cancelled parent context fails everything.
func (r *Readiness) Failed(parent context.Context) []string {
	ctx, cancel := context.WithTimeout(parent, ReadinessTimeout)
	defer cancel()
	var mu sync.Mutex
	var failed []string
	var wg sync.WaitGroup
	for name, check := range r.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan error, 1)
			go func() {
				defer func() {
					if recover() != nil {
						done <- errors.New("panic")
					}
				}()
				done <- check(ctx)
			}()
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				err = ctx.Err()
			}
			if err != nil {
				mu.Lock()
				failed = append(failed, name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(failed)
	return failed
}

type healthBody struct {
	Status string   `json:"status"`
	Failed []string `json:"failed,omitempty"`
}

func writeHealth(w http.ResponseWriter, code int, b healthBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(b)
}

// LiveHandler performs no dependency calls.
func LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, healthBody{Status: "live"})
	})
}

// ReadyHandler reports 200 only when every dependency answers in time.
func (r *Readiness) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if failed := r.Failed(req.Context()); len(failed) > 0 {
			writeHealth(w, http.StatusServiceUnavailable, healthBody{Status: "not_ready", Failed: failed})
			return
		}
		writeHealth(w, http.StatusOK, healthBody{Status: "ready"})
	})
}

// OperatorMux serves /metrics and health only; it never routes /v1.
func OperatorMux(m *Metrics, r *Readiness) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.Handle("GET /health/live", LiveHandler())
	mux.Handle("GET /health/ready", r.ReadyHandler())
	return mux
}

// Server is the dedicated operator listener.
type Server struct{ srv *http.Server }

func NewServer(addr string, handler http.Handler) *Server {
	return &Server{srv: &http.Server{
		Addr: addr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}}
}

// Run blocks until ctx is canceled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("observability: listen: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(shutdown)
}

// NotReadyError names the failed components; its text is bounded and safe to
// expose (no dependency error detail).
type NotReadyError struct{ Failed []string }

func (e *NotReadyError) Error() string { return "not ready: " + strings.Join(e.Failed, ",") }

// Check returns nil when ready or a *NotReadyError. It lets the developer
// API reuse the same bounded readiness logic.
func (r *Readiness) Check(ctx context.Context) error {
	if failed := r.Failed(ctx); len(failed) > 0 {
		return &NotReadyError{Failed: failed}
	}
	return nil
}
