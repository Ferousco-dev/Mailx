package api

import (
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/auth"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/webhook"
)

type routeServices struct {
	domains  *maildomain.Service
	webhooks *webhook.Service
}

// newMux registers every /v1 route plus health checks. Handlers stay
// agnostic of HOW the tenant/scopes were determined — authenticateMiddleware
// is the only place that decision is made (see authmiddleware.go); each
// route separately declares the scope it requires via requireScope, since
// different routes need different permissions.
func newMux(h *emailHandler, authSvc authService, readiness func() error, extras ...routeServices) *http.ServeMux {
	mux := http.NewServeMux()
	domainService := maildomain.NewService(h.db, maildomain.NewNetTXTResolver())
	box, _ := webhook.NewSecretBox(make([]byte, 32))
	webhookService, _ := webhook.NewService(h.db, box, webhook.URLPolicy{})
	if len(extras) > 0 {
		if extras[0].domains != nil {
			domainService = extras[0].domains
		}
		if extras[0].webhooks != nil {
			webhookService = extras[0].webhooks
		}
	}
	domains := newDomainHandler(domainService)
	webhooks := &webhookHandler{service: webhookService, db: h.db}

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/emails", requireScope(auth.ScopeEmailsSend)(h.handleSend))
	v1.HandleFunc("GET /v1/emails/{id}", requireScope(auth.ScopeEmailsRead)(h.handleGet))
	v1.HandleFunc("GET /v1/emails", requireScope(auth.ScopeEmailsRead)(h.handleList))
	v1.HandleFunc("POST /v1/domains", requireScope(auth.ScopeDomainsWrite)(domains.handleCreate))
	v1.HandleFunc("GET /v1/domains/{id}", requireScope(auth.ScopeDomainsRead)(domains.handleGet))
	v1.HandleFunc("GET /v1/domains", requireScope(auth.ScopeDomainsRead)(domains.handleList))
	v1.HandleFunc("POST /v1/domains/{id}/verify", requireScope(auth.ScopeDomainsWrite)(domains.handleVerify))
	v1.HandleFunc("DELETE /v1/domains/{id}", requireScope(auth.ScopeDomainsWrite)(domains.handleDelete))
	v1.HandleFunc("POST /v1/webhooks", requireScope(auth.ScopeWebhooksWrite)(webhooks.handleCreate))
	v1.HandleFunc("GET /v1/webhooks", requireScope(auth.ScopeWebhooksRead)(webhooks.handleList))
	v1.HandleFunc("GET /v1/webhooks/{id}", requireScope(auth.ScopeWebhooksRead)(webhooks.handleGet))
	v1.HandleFunc("DELETE /v1/webhooks/{id}", requireScope(auth.ScopeWebhooksWrite)(webhooks.handleDelete))
	v1.HandleFunc("POST /v1/webhooks/{id}/rotate-secret", requireScope(auth.ScopeWebhooksWrite)(webhooks.handleRotate))
	v1.HandleFunc("GET /v1/webhooks/{id}/deliveries", requireScope(auth.ScopeWebhooksRead)(webhooks.handleDeliveries))
	v1.HandleFunc("GET /v1/events", requireScope(auth.ScopeWebhooksRead)(webhooks.handleEvents))

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
