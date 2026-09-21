package api

import (
	"errors"
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/dmarc"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/spf"
	"github.com/Ferousco-dev/mailx/internal/webhook"
)

type routeServices struct {
	domains  *maildomain.Service
	webhooks *webhook.Service
	dkim     *dkim.Service
	spf      *spf.Service
	dmarc    *dmarc.Service
	metrics  *observability.Metrics
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
	if len(extras) > 0 && extras[0].dkim != nil {
		h.dkim = extras[0].dkim
	}
	dkimHandler := &dkimHandler{service: h.dkim}
	suppressions := &suppressionHandler{db: h.db}
	if len(extras) > 0 {
		suppressions.metrics = extras[0].metrics
	}
	spfHandler := &spfHandler{}
	dmarcHandler := &dmarcHandler{}
	if len(extras) > 0 {
		spfHandler.service = extras[0].spf
		dmarcHandler.service = extras[0].dmarc
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
	v1.HandleFunc("GET /v1/domains/{id}/dkim", requireScope(auth.ScopeDomainsRead)(dkimHandler.handleStatus))
	v1.HandleFunc("POST /v1/domains/{id}/dkim", requireScope(auth.ScopeDomainsWrite)(dkimHandler.handleCreate))
	v1.HandleFunc("POST /v1/domains/{id}/dkim/verify", requireScope(auth.ScopeDomainsWrite)(dkimHandler.handleVerify))
	v1.HandleFunc("GET /v1/domains/{id}/spf", requireScope(auth.ScopeDomainsRead)(spfHandler.handleGet))
	v1.HandleFunc("POST /v1/domains/{id}/spf/verify", requireScope(auth.ScopeDomainsWrite)(spfHandler.handleVerify))
	v1.HandleFunc("GET /v1/domains/{id}/dmarc", requireScope(auth.ScopeDomainsRead)(dmarcHandler.handleGet))
	v1.HandleFunc("POST /v1/domains/{id}/dmarc/verify", requireScope(auth.ScopeDomainsWrite)(dmarcHandler.handleVerify))
	v1.HandleFunc("POST /v1/suppressions", requireScope(auth.ScopeSuppressionsWrite)(suppressions.handleCreate))
	v1.HandleFunc("GET /v1/suppressions", requireScope(auth.ScopeSuppressionsRead)(suppressions.handleList))
	v1.HandleFunc("GET /v1/suppressions/{id}", requireScope(auth.ScopeSuppressionsRead)(suppressions.handleGet))
	v1.HandleFunc("DELETE /v1/suppressions/{id}", requireScope(auth.ScopeSuppressionsWrite)(suppressions.handleDelete))
	v1.HandleFunc("POST /v1/webhooks", requireScope(auth.ScopeWebhooksWrite)(webhooks.handleCreate))
	v1.HandleFunc("GET /v1/webhooks", requireScope(auth.ScopeWebhooksRead)(webhooks.handleList))
	v1.HandleFunc("GET /v1/webhooks/{id}", requireScope(auth.ScopeWebhooksRead)(webhooks.handleGet))
	v1.HandleFunc("DELETE /v1/webhooks/{id}", requireScope(auth.ScopeWebhooksWrite)(webhooks.handleDelete))
	v1.HandleFunc("POST /v1/webhooks/{id}/rotate-secret", requireScope(auth.ScopeWebhooksWrite)(webhooks.handleRotate))
	v1.HandleFunc("GET /v1/webhooks/{id}/deliveries", requireScope(auth.ScopeWebhooksRead)(webhooks.handleDeliveries))
	v1.HandleFunc("GET /v1/events", requireScope(auth.ScopeWebhooksRead)(webhooks.handleEvents))

	authenticated := chain(recordRoute(v1), authenticateMiddleware(authSvc))
	mux.Handle("/v1/", authenticated)

	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := readiness(); err != nil {
			// Raw dependency errors never reach clients; only bounded
			// component names (e.g. "postgres", "redis") do.
			body := map[string]any{"status": "not_ready"}
			var notReady *observability.NotReadyError
			if errors.As(err, &notReady) {
				body["failed"] = notReady.Failed
			}
			writeJSON(w, http.StatusServiceUnavailable, body)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	mux.HandleFunc("GET /openapi.json", serveOpenAPI)
	mux.HandleFunc("GET /docs", serveDocs)

	return mux
}
