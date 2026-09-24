package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func setupTrackingMux(t *testing.T) (http.Handler, *database.DB, string) {
	t.Helper()
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	verifyTestDomain(t, db, tenant.ID, "example.com")
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, store)
	h.trackingSecret = []byte("s3cret")
	h.trackingBaseURL = "https://track.example.com"
	authSvc := auth.NewService(db, nil)
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "k", []string{string(auth.ScopeEmailsSend)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	track := &trackHandler{db: db, secret: h.trackingSecret}
	mux := newMux(h, authSvc, func() error { return nil }, routeServices{track: track})
	return authInjector{next: mux, token: gen.Raw}, db, tenant.ID
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func insertRealMessage(t *testing.T, db *database.DB, tenantID string) string {
	t.Helper()
	id, err := storage.NewID()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := db.InsertMessage(context.Background(), database.NewMessage{
		ID: id, TenantID: tenantID, MailFrom: "<a@example.com>", MessageIDHeader: "<x@example.com>",
		Recipients: []database.RecipientInput{{Address: "<bob@example.com>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg.ID
}

func TestTrackClickRedirectsAndRecordsEvent(t *testing.T) {
	mux, db, tenantID := setupTrackingMux(t)
	msgID := insertRealMessage(t, db, tenantID)
	h := &emailHandler{trackingSecret: []byte("s3cret"), trackingBaseURL: "https://track.example.com"}
	html, err := h.injectTracking(tenantID, msgID, "bob@example.com", `<a href="https://example.com/page">go</a>`, false, true)
	if err != nil {
		t.Fatal(err)
	}
	start := indexOf(html, "/track/click/")
	token := html[start+len("/track/click/"):]
	token = token[:indexOf(token, `"`)]

	rec := doRaw(t, mux, "GET", "/track/click/"+token, "", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://example.com/page" {
		t.Fatalf("unexpected redirect: %q", loc)
	}
	events, err := db.ListMessageEvents(context.Background(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == database.EventClicked {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a clicked event, got %+v", events)
	}
}

func TestTrackOpenRecordsEventAndServesPixel(t *testing.T) {
	mux, db, tenantID := setupTrackingMux(t)
	msgID := insertRealMessage(t, db, tenantID)
	h := &emailHandler{trackingSecret: []byte("s3cret"), trackingBaseURL: "https://track.example.com"}
	html, err := h.injectTracking(tenantID, msgID, "bob@example.com", "<html><body>hi</body></html>", true, false)
	if err != nil {
		t.Fatal(err)
	}
	start := indexOf(html, "/track/open/")
	token := html[start+len("/track/open/"):]
	token = token[:indexOf(token, `"`)]

	rec := doRaw(t, mux, "GET", "/track/open/"+token, "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("got %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	events, err := db.ListMessageEvents(context.Background(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == database.EventOpened {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an opened event, got %+v", events)
	}
}

func TestTrackClickInvalidTokenNotFound(t *testing.T) {
	mux, _, _ := setupTrackingMux(t)
	rec := doRaw(t, mux, "GET", "/track/click/bogus", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}
