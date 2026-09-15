package api

import (
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/auth"
)

// newMux registers every /v1 route plus health checks. Handlers stay
// agnostic of HOW the tenant/scopes were determined — authenticateMiddleware
// is the only place that decision is made (see authmiddleware.go); each
// route separately declares the scope it requires via requireScope, since
// different routes need different permissions.
func newMux(h *emailHandler, authSvc authService, readiness func() error) *http.ServeMux {
	mux := http.NewServeMux()

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/emails", requireScope(auth.ScopeEmailsSend)(h.handleSend))
	v1.HandleFunc("GET /v1/emails/{id}", requireScope(auth.ScopeEmailsRead)(h.handleGet))
	v1.HandleFunc("GET /v1/emails", requireScope(auth.ScopeEmailsRead)(h.handleList))

	authenticated := chain(v1, authenticateMiddleware(authSvc))
	mux.Handle("/v1/", authenticated)

	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := readiness(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	mux.HandleFunc("GET /openapi.json", serveOpenAPI)
	mux.HandleFunc("GET /docs", serveDocs)

	return mux
}
