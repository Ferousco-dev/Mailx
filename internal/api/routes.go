package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/bimi"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/dmarc"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/humanauth"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/spf"
	"github.com/Ferousco-dev/mailx/internal/webhook"
)

type routeServices struct {
	domains   *maildomain.Service
	webhooks  *webhook.Service
	dkim      *dkim.Service
	spf       *spf.Service
	dmarc     *dmarc.Service
	bimi      *bimi.Service
	metrics   *observability.Metrics
	abuse     *AbuseControls
	feedback  *feedbackHandler   // nil disables the ingestion route
	track     *trackHandler      // nil disables /track routes
	humanAuth *humanauth.Service // nil disables /v1/auth/* and /v1/orgs
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
	bimiHandler := &bimiHandler{}
	if len(extras) > 0 {
		spfHandler.service = extras[0].spf
		dmarcHandler.service = extras[0].dmarc
		bimiHandler.service = extras[0].bimi
	}
	domains := newDomainHandler(domainService)
	webhooks := &webhookHandler{service: webhookService, db: h.db}

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/emails", requireScope(auth.ScopeEmailsSend)(h.handleSend))
	v1.HandleFunc("POST /v1/emails/batch", requireScope(auth.ScopeEmailsSend)(h.handleSendBatch))
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
	v1.HandleFunc("GET /v1/domains/{id}/bimi", requireScope(auth.ScopeDomainsRead)(bimiHandler.handleGet))
	v1.HandleFunc("POST /v1/domains/{id}/bimi/verify", requireScope(auth.ScopeDomainsWrite)(bimiHandler.handleVerify))
	broadcasts := &broadcastHandler{db: h.db, now: func() time.Time { return time.Now().UTC() }}
	v1.HandleFunc("POST /v1/broadcasts", requireScope(auth.ScopeBroadcastsWrite)(broadcasts.handleCreate))
	v1.HandleFunc("GET /v1/broadcasts", requireScope(auth.ScopeBroadcastsRead)(broadcasts.handleList))
	v1.HandleFunc("GET /v1/broadcasts/{id}", requireScope(auth.ScopeBroadcastsRead)(broadcasts.handleGet))
	v1.HandleFunc("GET /v1/broadcasts/{id}/recipients", requireScope(auth.ScopeBroadcastsRead)(broadcasts.handleListRecipients))

	analytics := &analyticsHandler{db: h.db, now: func() time.Time { return time.Now().UTC() }}
	v1.HandleFunc("GET /v1/analytics/overview", requireScope(auth.ScopeAnalyticsRead)(analytics.handleOverview))
	v1.HandleFunc("GET /v1/analytics/timeseries", requireScope(auth.ScopeAnalyticsRead)(analytics.handleTimeseries))
	v1.HandleFunc("GET /v1/analytics/broadcasts/{id}", requireScope(auth.ScopeAnalyticsRead)(analytics.handleBroadcast))
	v1.HandleFunc("GET /v1/analytics/domains", requireScope(auth.ScopeAnalyticsRead)(analytics.handleDomains))

	audiences := &audienceHandler{db: h.db}
	v1.HandleFunc("POST /v1/audiences", requireScope(auth.ScopeAudiencesWrite)(audiences.handleCreate))
	v1.HandleFunc("GET /v1/audiences", requireScope(auth.ScopeAudiencesRead)(audiences.handleList))
	v1.HandleFunc("GET /v1/audiences/{id}", requireScope(auth.ScopeAudiencesRead)(audiences.handleGet))
	v1.HandleFunc("PATCH /v1/audiences/{id}", requireScope(auth.ScopeAudiencesWrite)(audiences.handleUpdate))
	v1.HandleFunc("DELETE /v1/audiences/{id}", requireScope(auth.ScopeAudiencesWrite)(audiences.handleDelete))
	v1.HandleFunc("POST /v1/audiences/{id}/contacts", requireScope(auth.ScopeAudiencesWrite)(audiences.handleAddMember))
	v1.HandleFunc("GET /v1/audiences/{id}/contacts", requireScope(auth.ScopeAudiencesRead)(audiences.handleListMembers))
	v1.HandleFunc("DELETE /v1/audiences/{id}/contacts/{contact_id}", requireScope(auth.ScopeAudiencesWrite)(audiences.handleRemoveMember))

	contacts := &contactHandler{db: h.db}
	v1.HandleFunc("POST /v1/contacts", requireScope(auth.ScopeContactsWrite)(contacts.handleCreate))
	v1.HandleFunc("GET /v1/contacts", requireScope(auth.ScopeContactsRead)(contacts.handleList))
	v1.HandleFunc("GET /v1/contacts/{id}", requireScope(auth.ScopeContactsRead)(contacts.handleGet))
	v1.HandleFunc("PATCH /v1/contacts/{id}", requireScope(auth.ScopeContactsWrite)(contacts.handleUpdate))
	v1.HandleFunc("DELETE /v1/contacts/{id}", requireScope(auth.ScopeContactsWrite)(contacts.handleDelete))

	templates := &templateHandler{db: h.db}
	v1.HandleFunc("POST /v1/templates", requireScope(auth.ScopeTemplatesWrite)(templates.handleCreate))
	v1.HandleFunc("GET /v1/templates", requireScope(auth.ScopeTemplatesRead)(templates.handleList))
	v1.HandleFunc("GET /v1/templates/{id}", requireScope(auth.ScopeTemplatesRead)(templates.handleGet))
	v1.HandleFunc("PATCH /v1/templates/{id}", requireScope(auth.ScopeTemplatesWrite)(templates.handleUpdate))
	v1.HandleFunc("DELETE /v1/templates/{id}", requireScope(auth.ScopeTemplatesWrite)(templates.handleDelete))
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

	var abuse *AbuseControls
	if len(extras) > 0 {
		abuse = extras[0].abuse
	}
	// Order matters: authenticate first (the limiter needs the tenant and key),
	// then the request limiter, then the route. Unauthenticated requests are
	// rejected before they can touch a tenant bucket.
	authenticated := chain(recordRoute(v1), authenticateMiddleware(authSvc), requestLimitMiddleware(abuse))
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

	if len(extras) > 0 && extras[0].humanAuth != nil {
		// Human/browser session endpoints: a clearly separate path prefix
		// (/v1/auth/*, /v1/orgs) and a clearly separate auth mechanism
		// (JWT, not API key) from the tenant-facing v1 mux above. Registered
		// directly on mux (not the "authenticated" chain) — ServeMux
		// prefers the more specific "/v1/auth/" and "/v1/orgs" patterns
		// over the "/v1/" catch-all, so these never pass through
		// authenticateMiddleware/requireScope.
		ha := &humanAuthHandler{svc: extras[0].humanAuth}
		// IP-keyed rate limit: this surface is unauthenticated by
		// definition (no tenant/API key exists yet), so it never passes
		// through requestLimitMiddleware above — see authIPLimitMiddleware's
		// doc for why that would otherwise leave password-guessing/
		// signup-flooding completely unthrottled.
		mux.Handle("POST /v1/auth/signup", chain(http.HandlerFunc(ha.handleSignup), authIPLimitMiddleware(abuse)))
		mux.Handle("POST /v1/auth/login", chain(http.HandlerFunc(ha.handleLogin), authIPLimitMiddleware(abuse)))
		mux.Handle("POST /v1/auth/refresh", chain(http.HandlerFunc(ha.handleRefresh), authIPLimitMiddleware(abuse)))
		mux.HandleFunc("POST /v1/auth/logout", ha.handleLogout)
		orgsAuthenticated := humanAuthMiddleware(extras[0].humanAuth)
		mux.Handle("POST /v1/orgs", orgsAuthenticated(http.HandlerFunc(ha.handleCreateOrg)))
		mux.Handle("GET /v1/orgs", orgsAuthenticated(http.HandlerFunc(ha.handleListOrgs)))
	}
	if len(extras) > 0 && extras[0].feedback != nil {
		// Deliberately NOT under /v1 and NOT authenticateMiddleware: this is the
		// operator-only feedback ingestion boundary (see feedback_handler.go),
		// not a tenant-facing route.
		mux.HandleFunc("POST /internal/feedback", extras[0].feedback.handleIngest)
	}
	if len(extras) > 0 && extras[0].track != nil {
		mux.HandleFunc("GET /track/open/{token}", extras[0].track.handleOpen)
		mux.HandleFunc("GET /track/click/{token}", extras[0].track.handleClick)
	}
	mux.HandleFunc("GET /openapi.json", serveOpenAPI)
	mux.HandleFunc("GET /docs", serveDocs)

	return mux
}
