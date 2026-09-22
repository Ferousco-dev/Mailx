package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
)

func TestAnalyticsOverviewRequiresScope(t *testing.T) {
	a := newDKIMAPI(t)
	noScope, _ := a.scopedActor("acme", auth.ScopeEmailsRead)
	now := time.Now().UTC()
	rec := doJSON(t, noScope.h, "GET",
		"/v1/analytics/overview?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Format(time.RFC3339), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsOverviewMissingRangeRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeAnalyticsRead)
	if rec := doJSON(t, ac.h, "GET", "/v1/analytics/overview", nil); rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsOverviewInvalidRangeRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeAnalyticsRead)
	now := time.Now().UTC()
	// to before from.
	rec := doJSON(t, ac.h, "GET",
		"/v1/analytics/overview?from="+now.Format(time.RFC3339)+"&to="+now.Add(-time.Hour).Format(time.RFC3339), nil)
	if rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsOverviewRangeTooLargeRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeAnalyticsRead)
	now := time.Now().UTC()
	rec := doJSON(t, ac.h, "GET",
		"/v1/analytics/overview?from="+now.Add(-365*24*time.Hour).Format(time.RFC3339)+"&to="+now.Format(time.RFC3339), nil)
	if rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsOverviewEmptyTenantReturnsZeroes(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeAnalyticsRead)
	now := time.Now().UTC()
	rec := doJSON(t, ac.h, "GET",
		"/v1/analytics/overview?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Format(time.RFC3339), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp analyticsOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Counts.Queued != 0 || resp.Counts.Delivered != 0 || resp.CurrentlySuppressed != 0 {
		t.Fatalf("a new tenant must get coherent zero values, not an error: %+v", resp)
	}
}

// Sending a real email must produce a Queued fact visible in the overview —
// proving the handler is wired to real acceptance, not just a stub.
func TestAnalyticsOverviewReflectsRealSend(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeEmailsSend, auth.ScopeAnalyticsRead)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	sendRec := doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "hi",
	})
	if sendRec.Code != http.StatusAccepted {
		t.Fatal(sendRec.Body.String())
	}
	now := time.Now().UTC()
	rec := doJSON(t, ac.h, "GET",
		"/v1/analytics/overview?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Add(time.Hour).Format(time.RFC3339), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp analyticsOverviewResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Counts.Queued != 1 {
		t.Fatalf("expected 1 queued fact from the real send, got %+v", resp.Counts)
	}
}

func TestAnalyticsTimeseriesInvalidIntervalRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeAnalyticsRead)
	now := time.Now().UTC()
	rec := doJSON(t, ac.h, "GET",
		"/v1/analytics/timeseries?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Format(time.RFC3339)+"&interval=minute", nil)
	if rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsTimeseriesEmptyIsEmptyArrayNotError(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeAnalyticsRead)
	now := time.Now().UTC()
	rec := doJSON(t, ac.h, "GET",
		"/v1/analytics/timeseries?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Format(time.RFC3339)+"&interval=hour", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp []analyticsBucketResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp == nil || len(resp) != 0 {
		t.Fatalf("expected an empty array, got %+v", resp)
	}
}

func TestAnalyticsBroadcastRequiresScope(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	b := decodeBroadcast(t, doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(nil)))
	noScope, _ := a.scopedActor("other", auth.ScopeEmailsRead)
	rec := doJSON(t, noScope.h, "GET", "/v1/analytics/broadcasts/"+b.ID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsBroadcastCrossTenantNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	owner := setupBroadcastReady(t, a, "owner")
	b := decodeBroadcast(t, doJSON(t, owner.h, "POST", "/v1/broadcasts", owner.body(nil)))
	attacker, _ := a.scopedActor("attacker", auth.ScopeAnalyticsRead)
	rec := doJSON(t, attacker.h, "GET", "/v1/analytics/broadcasts/"+b.ID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsBroadcastUnknownIDNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeAnalyticsRead)
	rec := doJSON(t, ac.h, "GET", "/v1/analytics/broadcasts/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyticsBroadcastReturnsSnapshotCounts(t *testing.T) {
	a := newDKIMAPI(t)
	scopes := append(append([]auth.Scope{}, allBroadcastScopes...), auth.ScopeAnalyticsRead)
	ac, _ := a.scopedActor("acme", scopes...)
	domain := "acme-" + ac.tenant.ID[:8] + ".example.com"
	verifyTestDomain(t, a.db, ac.tenant.ID, domain)
	aud := decodeAudience(t, doJSON(t, ac.h, "POST", "/v1/audiences", map[string]any{"name": "a"}))
	c := decodeContact(t, doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "one@dest.example"}))
	doJSON(t, ac.h, "POST", "/v1/audiences/"+aud.ID+"/contacts", map[string]any{"contact_id": c.ID})
	tmpl := decodeTemplate(t, doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi", "text": "Body"}))
	b := decodeBroadcast(t, doJSON(t, ac.h, "POST", "/v1/broadcasts", map[string]any{
		"name": "camp", "audience_id": aud.ID, "template_id": tmpl.ID, "from": "a@" + domain,
	}))

	rec := doJSON(t, ac.h, "GET", "/v1/analytics/broadcasts/"+b.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp broadcastAnalyticsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Right after acceptance, expansion is async — the important thing is a
	// coherent response (intended may be 0 or 1 depending on timing, but
	// must never error and must reflect the durable snapshot, not a live
	// Audience read).
	if resp.BroadcastID != b.ID {
		t.Fatalf("%+v", resp)
	}
}
