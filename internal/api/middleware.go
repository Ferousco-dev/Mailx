package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/observability"
)

// newRequestID always generates the canonical, server-owned request ID —
// a client-supplied X-Request-Id is echoed back in logs for correlation
// but is NEVER trusted as this value, so it can never be used to spoof or
// guess another request's identity.
func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// requestMeta is filled in as a request moves through the handler chain so
// the outermost middleware can log the matched route and authenticated
// tenant after the (context-copying) inner middleware have run.
type requestMeta struct {
	pattern  string
	tenantID string
}

type metaKey struct{}

func metaFromContext(ctx context.Context) *requestMeta {
	m, _ := ctx.Value(metaKey{}).(*requestMeta)
	return m
}

// recordRoute captures the /v1 sub-mux's matched pattern. mux sets
// r.Pattern on the request it is given, which is the pointer we hold here.
func recordRoute(next *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if m := metaFromContext(r.Context()); m != nil {
			m.pattern = r.Pattern
		}
	})
}

// routeLabel turns a ServeMux pattern into a bounded route label: the method
// prefix is dropped and unmatched requests collapse to one value, so
// attacker-controlled paths and queries can never reach logs or labels.
func routeLabel(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	if pattern == "" {
		return "unmatched"
	}
	return pattern
}

// withRequestIDMiddleware is the OUTERMOST middleware: it assigns every
// request a server-generated ID (exposed as X-Request-Id), and records one
// access log line plus metrics after the handler chain finishes — including
// after a recovered panic. It never logs headers, query strings, or bodies.
func withRequestIDMiddleware(log *slog.Logger, metrics *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newRequestID()
			w.Header().Set("X-Request-Id", id)
			meta := &requestMeta{}
			ctx := context.WithValue(withRequestID(r.Context(), id), metaKey{}, meta)
			r = r.WithContext(ctx)

			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			elapsed := time.Since(start)

			pattern := meta.pattern
			if pattern == "" {
				pattern = r.Pattern
			}
			route := routeLabel(pattern)
			attrs := []any{"request_id", id, "method", r.Method, "route", route,
				"status", sw.status, "duration_ms", elapsed.Milliseconds()}
			if meta.tenantID != "" {
				attrs = append(attrs, "tenant_id", meta.tenantID)
			}
			log.Info("http_request", attrs...)
			metrics.HTTPRequest(r.Method, route, sw.status, elapsed)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the real writer (Flush,
// deadlines) through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withRecoverMiddleware stops one panicking handler from crashing the
// whole server; the client gets a generic 500. The panic value is not
// logged (it could contain request-derived data); the request ID is enough
// to find the stack via the access log and to correlate the client's error.
func withRecoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("http_panic", "request_id", requestIDFromContext(r.Context()))
					writeError(w, r, newError(ErrInternal, "internal_error", "an internal error occurred"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// maxBodyBytes bounds every /v1 request body; JSON decoding an unbounded
// body is an easy memory-exhaustion vector.
const maxBodyBytes = 5 << 20 // 5 MiB

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
