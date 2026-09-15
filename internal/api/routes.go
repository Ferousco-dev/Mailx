package api

import "net/http"

// newMux registers every /v1 route plus health checks. Handlers are kept
// tenant-agnostic of HOW the tenant was determined — devTenantMiddleware
// is the only place that decision is made (see middleware.go).
func newMux(h *emailHandler, tenantID string, readiness func() error) *http.ServeMux {
	mux := http.NewServeMux()

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/emails", h.handleSend)
	v1.HandleFunc("GET /v1/emails/{id}", h.handleGet)
	v1.HandleFunc("GET /v1/emails", h.handleList)

	tenantScoped := chain(v1, devTenantMiddleware(tenantID))
	mux.Handle("/v1/", tenantScoped)

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
