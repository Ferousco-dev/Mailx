package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
)

var allTemplateScopes = []auth.Scope{auth.ScopeTemplatesRead, auth.ScopeTemplatesWrite, auth.ScopeEmailsSend, auth.ScopeEmailsRead}

func decodeTemplate(t *testing.T, rec *httptest.ResponseRecorder) templateResource {
	t.Helper()
	var r templateResource
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return r
}

func TestTemplateCRUD(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)

	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{
		"name": "welcome", "subject": "Welcome, {{name}}", "text": "Hi {{name}}, welcome to {{product}}.", "html": "<h1>Hi {{name}}</h1>",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	created := decodeTemplate(t, rec)
	if created.ID == "" || created.Name != "welcome" || created.CreatedAt.IsZero() {
		t.Fatalf("%+v", created)
	}

	if rec = doJSON(t, ac.h, "GET", "/v1/templates/"+created.ID, nil); rec.Code != 200 || decodeTemplate(t, rec).ID != created.ID {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, ac.h, "GET", "/v1/templates", nil)
	var list templateList
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, ac.h, "PATCH", "/v1/templates/"+created.ID, map[string]any{"subject": "New subject"})
	updated := decodeTemplate(t, rec)
	if rec.Code != 200 || updated.Subject != "New subject" || updated.Text != created.Text {
		t.Fatalf("%d %+v", rec.Code, updated)
	}

	if rec = doJSON(t, ac.h, "DELETE", "/v1/templates/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("%d", rec.Code)
	}
	if rec = doJSON(t, ac.h, "GET", "/v1/templates/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: %d", rec.Code)
	}
	if rec = doJSON(t, ac.h, "PATCH", "/v1/templates/"+created.ID, map[string]any{"name": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH after delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, ac.h, "DELETE", "/v1/templates/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE after delete: %d", rec.Code)
	}
}

func TestTemplateDuplicateNameConflict(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	body := map[string]any{"name": "dup", "subject": "s", "text": "t"}
	if rec := doJSON(t, ac.h, "POST", "/v1/templates", body); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	if rec := doJSON(t, ac.h, "POST", "/v1/templates", body); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestTemplateValidation(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	cases := map[string]any{
		"":     map[string]any{"name": "", "subject": "s", "text": "t"},
		"body": map[string]any{"name": "n", "subject": "s"},
	}
	for name, body := range cases {
		if rec := doJSON(t, ac.h, "POST", "/v1/templates", body); rec.Code != 422 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestTemplateCrossTenantNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	ac1, _ := a.scopedActor("acme", allTemplateScopes...)
	ac2, _ := a.scopedActor("other", allTemplateScopes...)
	rec := doJSON(t, ac1.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "s", "text": "t"})
	created := decodeTemplate(t, rec)
	if rec := doJSON(t, ac2.h, "GET", "/v1/templates/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, ac2.h, "DELETE", "/v1/templates/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	// tenant 1's template survives tenant 2's delete attempt
	if rec := doJSON(t, ac1.h, "GET", "/v1/templates/"+created.ID, nil); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestTemplateRequiresScope(t *testing.T) {
	a := newDKIMAPI(t)
	readOnly, _ := a.scopedActor("acme", auth.ScopeTemplatesRead)
	if rec := doJSON(t, readOnly.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "s", "text": "t"}); rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------- send integration

func TestSendWithTemplateProducesRenderedMIME(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")

	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{
		"name": "welcome", "subject": "Welcome, {{name}}", "text": "Hi {{name}}, welcome to {{product}}.",
	})
	tmpl := decodeTemplate(t, rec)

	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID,
		"variables": map[string]string{"name": "Feranmi", "product": "MailX"},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := decodeEmail(t, rec)
	if got.Subject != "Welcome, Feranmi" {
		t.Fatalf("subject = %q", got.Subject)
	}
	full := doJSON(t, ac.h, "GET", "/v1/emails/"+got.ID, nil)
	fullEmail := decodeEmail(t, full)
	if fullEmail.Text == nil || *fullEmail.Text != "Hi Feranmi, welcome to MailX." {
		t.Fatalf("%+v", fullEmail)
	}
}

func TestSendTemplateAndContentConflict(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "s", "text": "t"})
	tmpl := decodeTemplate(t, rec)

	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID, "subject": "conflict",
	})
	if rec.Code != 422 || errCode(t, rec) != "template_and_content_conflict" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSendVariablesWithoutTemplateRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "hi", "variables": map[string]string{"x": "y"},
	})
	if rec.Code != 422 || errCode(t, rec) != "variables_without_template" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSendUnknownTemplateNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSendTemplateFromAnotherTenantNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	owner, _ := a.scopedActor("owner", allTemplateScopes...)
	attacker, _ := a.scopedActor("attacker", allTemplateScopes...)
	verifyTestDomain(t, a.db, attacker.tenant.ID, "example.com")
	rec := doJSON(t, owner.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "s", "text": "t"})
	tmpl := decodeTemplate(t, rec)
	rec = doJSON(t, attacker.h, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSendDeletedTemplateCannotBeUsed(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "s", "text": "t"})
	tmpl := decodeTemplate(t, rec)
	doJSON(t, ac.h, "DELETE", "/v1/templates/"+tmpl.ID, nil)
	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// Header injection: the renderer does not strip CR/LF, but the existing MIME
// builder does (unaffected by templates) — end-to-end this must still be
// refused, not silently sanitized or accepted.
func TestSendTemplateSubjectCRLFInjectionRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi {{name}}", "text": "x"})
	tmpl := decodeTemplate(t, rec)
	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID,
		"variables": map[string]string{"name": "x\r\nBcc: attacker@example.com"},
	})
	if rec.Code < 400 {
		t.Fatalf("CRLF injection through a template variable was accepted: %d %s", rec.Code, rec.Body.String())
	}
}

// Editing a template AFTER acceptance must not change the already-accepted message.
func TestTemplateEditAfterSendDoesNotMutateAcceptedMessage(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi {{name}}", "text": "x"})
	tmpl := decodeTemplate(t, rec)
	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID, "variables": map[string]string{"name": "Original"},
	})
	sent := decodeEmail(t, rec)

	doJSON(t, ac.h, "PATCH", "/v1/templates/"+tmpl.ID, map[string]any{"subject": "Edited {{name}}"})

	rec = doJSON(t, ac.h, "GET", "/v1/emails/"+sent.ID, nil)
	got := decodeEmail(t, rec)
	if got.Subject != "Hi Original" {
		t.Fatalf("accepted message mutated by a later template edit: %q", got.Subject)
	}
}

// Idempotency: same key + template edited between original and retry replays
// the original accepted content, never a second message.
func TestSendTemplateIdempotencyAcrossTemplateEdit(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi {{name}}", "text": "x"})
	tmpl := decodeTemplate(t, rec)
	body := map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID, "variables": map[string]string{"name": "Original"}}

	first := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "k1", body)
	firstEmail := decodeEmail(t, first)

	doJSON(t, ac.h, "PATCH", "/v1/templates/"+tmpl.ID, map[string]any{"subject": "Changed {{name}}"})

	replay := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "k1", body)
	if replay.Code != http.StatusAccepted || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("%d %s", replay.Code, replay.Body.String())
	}
	replayEmail := decodeEmail(t, replay)
	if replayEmail.ID != firstEmail.ID || replayEmail.Subject != "Hi Original" {
		t.Fatalf("replay diverged: %+v vs %+v", firstEmail, replayEmail)
	}
}

func TestSendTemplateIdempotencyDifferentVariablesConflicts(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi {{name}}", "text": "x"})
	tmpl := decodeTemplate(t, rec)

	body1 := map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID, "variables": map[string]string{"name": "A"}}
	body2 := map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID, "variables": map[string]string{"name": "B"}}
	if rec := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "k2", body1); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body.String())
	}
	rec = doJSONWithKey(t, ac.h, "POST", "/v1/emails", "k2", body2)
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSendTemplateIdempotencyDifferentTemplateConflicts(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec1 := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t1", "subject": "s1", "text": "x"})
	rec2 := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t2", "subject": "s2", "text": "x"})
	t1, t2 := decodeTemplate(t, rec1), decodeTemplate(t, rec2)

	body1 := map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": t1.ID}
	body2 := map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": t2.ID}
	if rec := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "k3", body1); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body.String())
	}
	rec := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "k3", body2)
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestOpenAPIDocumentsTemplates(t *testing.T) {
	if !strings.Contains(openAPISpec, "/templates") {
		t.Fatal("OpenAPI spec does not document /v1/templates")
	}
}

// A send that loads/renders a template concurrently with the template being
// deleted must still produce a stable outcome: either it sees the template
// (and the resulting message is fixed forever after) or it gets 404. No
// partial/corrupt state either way.
func TestConcurrentSendAndDeleteIsStable(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi {{name}}", "text": "x"})
	tmpl := decodeTemplate(t, rec)

	done := make(chan struct{})
	go func() {
		doJSON(t, ac.h, "DELETE", "/v1/templates/"+tmpl.ID, nil)
		close(done)
	}()
	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID, "variables": map[string]string{"name": "x"},
	})
	<-done
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusAccepted {
		sent := decodeEmail(t, rec)
		get := doJSON(t, ac.h, "GET", "/v1/emails/"+sent.ID, nil)
		if decodeEmail(t, get).Subject != "Hi x" {
			t.Fatalf("accepted message content unstable: %s", get.Body.String())
		}
	}
}

// Malformed template syntax never breaks a send: unknown/broken {{...}} tokens
// render as literal text, per emailtemplate.Render's documented behavior.
func TestSendWithMalformedTemplateSyntaxRendersLiterally(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "Hi {{ }}", "text": "x"})
	tmpl := decodeTemplate(t, rec)
	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if decodeEmail(t, rec).Subject != "Hi {{ }}" {
		t.Fatalf("subject = %q", decodeEmail(t, rec).Subject)
	}
}

// Huge variable maps are rejected before rendering, never accepted with quiet truncation.
func TestSendWithOversizedVariablesRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allTemplateScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/templates", map[string]any{"name": "t", "subject": "s", "text": "x"})
	tmpl := decodeTemplate(t, rec)
	vars := map[string]string{}
	for i := 0; i < 60; i++ {
		vars[string(rune('a'+i%26))+string(rune(i))] = "v"
	}
	rec = doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"b@example.com"}, "template_id": tmpl.ID, "variables": vars})
	if rec.Code != 422 || errCode(t, rec) != "invalid_variables" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
