package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
)

var allBroadcastScopes = []auth.Scope{auth.ScopeBroadcastsRead, auth.ScopeBroadcastsWrite, auth.ScopeAudiencesRead, auth.ScopeAudiencesWrite, auth.ScopeContactsRead, auth.ScopeContactsWrite, auth.ScopeTemplatesRead, auth.ScopeTemplatesWrite}

func decodeBroadcast(t *testing.T, rec *httptest.ResponseRecorder) broadcastResource {
	t.Helper()
	var r broadcastResource
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return r
}

// broadcastFixture is one tenant with a verified sending domain, an audience
// (with one contact) and a template — ready for a broadcast create request.
type broadcastFixture struct {
	h                      http.Handler
	fromAddr               string
	audienceID, templateID string
}

func setupBroadcastReady(t *testing.T, a *dkimAPI, tenantName string) broadcastFixture {
	t.Helper()
	ac, _ := a.scopedActor(tenantName, allBroadcastScopes...)
	domain := tenantName + "-" + ac.tenant.ID[:8] + ".example.com"
	verifyTestDomain(t, a.db, ac.tenant.ID, domain)
	aud := decodeAudience(t, doJSON(t, ac.h, "POST", "/v1/audiences", map[string]any{"name": "a"}))
	c := decodeContact(t, doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "one@dest.example"}))
	doJSON(t, ac.h, "POST", "/v1/audiences/"+aud.ID+"/contacts", map[string]any{"contact_id": c.ID})
	tmpl := decodeTemplate(t, doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi {{name}}", "text": "Body"}))
	return broadcastFixture{h: ac.h, fromAddr: "a@" + domain, audienceID: aud.ID, templateID: tmpl.ID}
}

func (f broadcastFixture) body(overrides map[string]any) map[string]any {
	body := map[string]any{"name": "camp", "audience_id": f.audienceID, "template_id": f.templateID, "from": f.fromAddr}
	for k, v := range overrides {
		body[k] = v
	}
	return body
}

func TestBroadcastCreateAccepted(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(map[string]any{"name": "Sept Update"}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	b := decodeBroadcast(t, rec)
	if b.ID == "" || b.Status != "accepted" {
		t.Fatalf("%+v", b)
	}
	got := doJSON(t, f.h, "GET", "/v1/broadcasts/"+b.ID, nil)
	if got.Code != 200 || decodeBroadcast(t, got).ID != b.ID {
		t.Fatalf("%d %s", got.Code, got.Body.String())
	}
}

func TestBroadcastListsRecipientsBounded(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(nil))
	b := decodeBroadcast(t, rec)
	rec = doJSON(t, f.h, "GET", "/v1/broadcasts/"+b.ID+"/recipients", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var list broadcastRecipientList
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	// Expansion is async: the list may be empty right after acceptance — the
	// important thing is the endpoint responds correctly and is bounded, not
	// that it already has data (that's the expander's job, tested separately).
	if len(list.Data) > defaultLimit {
		t.Fatalf("%d rows, want <= %d", len(list.Data), defaultLimit)
	}
}

func TestBroadcastMissingNameOrAudienceOrTemplateRejected(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	cases := []map[string]any{
		{"audience_id": f.audienceID, "template_id": f.templateID, "from": f.fromAddr},
		{"name": "n", "template_id": f.templateID, "from": f.fromAddr},
		{"name": "n", "audience_id": f.audienceID, "from": f.fromAddr},
		{"name": "n", "audience_id": f.audienceID, "template_id": f.templateID},
	}
	for i, body := range cases {
		if rec := doJSON(t, f.h, "POST", "/v1/broadcasts", body); rec.Code != 422 {
			t.Fatalf("case %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
}

func TestBroadcastUnknownAudienceOrTemplateNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	if rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(map[string]any{"audience_id": "nope"})); rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(map[string]any{"template_id": "nope"})); rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastCrossTenantAudienceRejected(t *testing.T) {
	a := newDKIMAPI(t)
	owner := setupBroadcastReady(t, a, "owner")
	attacker := setupBroadcastReady(t, a, "attacker")
	rec := doJSON(t, attacker.h, "POST", "/v1/broadcasts", map[string]any{
		"name": "n", "audience_id": owner.audienceID, "template_id": attacker.templateID, "from": attacker.fromAddr,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastCrossTenantTemplateRejected(t *testing.T) {
	a := newDKIMAPI(t)
	owner := setupBroadcastReady(t, a, "owner")
	attacker := setupBroadcastReady(t, a, "attacker")
	rec := doJSON(t, attacker.h, "POST", "/v1/broadcasts", map[string]any{
		"name": "n", "audience_id": attacker.audienceID, "template_id": owner.templateID, "from": attacker.fromAddr,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastUnverifiedFromDomainForbidden(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(map[string]any{"from": "a@other-unverified.com"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastCrossTenantGetNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	owner := setupBroadcastReady(t, a, "owner")
	attacker := setupBroadcastReady(t, a, "attacker")
	b := decodeBroadcast(t, doJSON(t, owner.h, "POST", "/v1/broadcasts", owner.body(nil)))
	if rec := doJSON(t, attacker.h, "GET", "/v1/broadcasts/"+b.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastRequiresScope(t *testing.T) {
	a := newDKIMAPI(t)
	readOnly, _ := a.scopedActor("acme", auth.ScopeBroadcastsRead)
	if rec := doJSON(t, readOnly.h, "POST", "/v1/broadcasts", map[string]any{"name": "n", "audience_id": "x", "template_id": "y", "from": "a@example.com"}); rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// Idempotency: same key + same body replays the same broadcast; a template
// edit between the accepted request and a retry must not create a second
// campaign nor change what the retry returns.
func TestBroadcastIdempotencyReplay(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	body := f.body(nil)
	first := doJSONWithKey(t, f.h, "POST", "/v1/broadcasts", "k1", body)
	if first.Code != http.StatusAccepted {
		t.Fatal(first.Body.String())
	}
	b1 := decodeBroadcast(t, first)

	doJSON(t, f.h, "PATCH", "/v1/templates/"+f.templateID, map[string]any{"subject": "Changed"})

	replay := doJSONWithKey(t, f.h, "POST", "/v1/broadcasts", "k1", body)
	if replay.Code != http.StatusAccepted || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("%d %s", replay.Code, replay.Body.String())
	}
	b2 := decodeBroadcast(t, replay)
	if b1.ID != b2.ID {
		t.Fatalf("replay created a different broadcast: %s vs %s", b1.ID, b2.ID)
	}
}

func TestBroadcastIdempotencyDifferentBodyConflicts(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "acme")
	if rec := doJSONWithKey(t, f.h, "POST", "/v1/broadcasts", "k2", f.body(map[string]any{"name": "n1"})); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body.String())
	}
	rec := doJSONWithKey(t, f.h, "POST", "/v1/broadcasts", "k2", f.body(map[string]any{"name": "n2"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestOpenAPIDocumentsBroadcasts(t *testing.T) {
	if !strings.Contains(openAPISpec, "/broadcasts") {
		t.Fatal("OpenAPI spec does not document /v1/broadcasts")
	}
}
