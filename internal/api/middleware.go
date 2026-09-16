package api

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"time"
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

// withRequestIDMiddleware assigns every request a server-generated ID,
// exposes it as X-Request-Id, and logs method/route/status/duration once
// the handler completes — enough to debug a request without logging its
// body (see Handler.logRequest's doc for what is deliberately excluded).
func withRequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(withRequestID(r.Context(), id))

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("request_id=%s method=%s path=%s status=%d duration=%s",
			id, r.Method, r.URL.Path, sw.status, time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// withRecoverMiddleware stops one panicking handler from crashing the
// whole server; the client gets a generic 500, and the panic value (never
// request/body content) is logged for debugging.
func withRecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("request_id=%s panic: %v", requestIDFromContext(r.Context()), rec)
				writeError(w, r, newError(ErrInternal, "internal_error", "an internal error occurred"))
			}
		}()
		next.ServeHTTP(w, r)
	})
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
