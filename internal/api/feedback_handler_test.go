package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/feedback"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func newFeedbackMessage(t *testing.T, db *database.DB, tenantID string) database.Message {
	t.Helper()
	id, err := storage.NewID()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := db.InsertMessage(context.Background(), database.NewMessage{
		ID: id, TenantID: tenantID, MailFrom: "<a@example.com>", FromHeader: "a@example.com",
		Subject: "s", MessageIDHeader: "<" + id + "@mailx.local>",
		Recipients: []database.RecipientInput{{Address: "<bob@example.com>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestFeedbackIngestRequiresAuth(t *testing.T) {
	db := newTestDB(t)
	corr, _ := feedback.NewCorrelator([]byte("01234567890123456789012345678901"))
	fh := &feedbackHandler{db: db, correlator: corr, ingestToken: "secret-token"}
	mux := newMux(newEmailHandler(db, mustStore(t)), auth.NewService(db, nil), func() error { return nil }, routeServices{feedback: fh})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/internal/feedback", bytes.NewReader([]byte(`{}`)))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth header: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/internal/feedback", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer wrong")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rec.Code)
	}
}

func TestFeedbackIngestBounceProcessesAndSuppresses(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	msg := newFeedbackMessage(t, db, tn.ID)
	fh := &feedbackHandler{db: db, ingestToken: "secret-token"}
	mux := newMux(newEmailHandler(db, mustStore(t)), auth.NewService(db, nil), func() error { return nil }, routeServices{feedback: fh})

	raw := dsnMessageForAPITest("bob@example.com")
	body, _ := json.Marshal(map[string]any{
		"type": "dsn", "message_id": msg.ID, "raw": base64.StdEncoding.EncodeToString(raw),
	})
	req := httptest.NewRequest("POST", "/internal/feedback", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	sup, err := db.SuppressedForTenant(context.Background(), tn.ID, []string{"bob@example.com"})
	if err != nil || !sup["bob@example.com"] {
		t.Fatalf("not suppressed: %v %v", sup, err)
	}
}

func TestFeedbackIngestUnresolvableCorrelation(t *testing.T) {
	db := newTestDB(t)
	fh := &feedbackHandler{db: db, ingestToken: "secret-token"}
	mux := newMux(newEmailHandler(db, mustStore(t)), auth.NewService(db, nil), func() error { return nil }, routeServices{feedback: fh})

	body, _ := json.Marshal(map[string]any{"type": "dsn"})
	req := httptest.NewRequest("POST", "/internal/feedback", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestFeedbackIngestCrossTenantCannotForgeMessageID(t *testing.T) {
	db := newTestDB(t)
	a := newTestTenant(t, db)
	b := newTestTenant(t, db)
	msgA := newFeedbackMessage(t, db, a.ID)
	fh := &feedbackHandler{db: db, ingestToken: "secret-token"}
	mux := newMux(newEmailHandler(db, mustStore(t)), auth.NewService(db, nil), func() error { return nil }, routeServices{feedback: fh})

	raw := dsnMessageForAPITest("bob@example.com")
	body, _ := json.Marshal(map[string]any{"type": "dsn", "message_id": msgA.ID, "raw": base64.StdEncoding.EncodeToString(raw)})
	req := httptest.NewRequest("POST", "/internal/feedback", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d", rec.Code)
	}
	// tenant B is never touched even though the request went through
	supB, _ := db.SuppressedForTenant(context.Background(), b.ID, []string{"bob@example.com"})
	if supB["bob@example.com"] {
		t.Fatal("cross-tenant leak")
	}
}

func dsnMessageForAPITest(recipient string) []byte {
	boundary := "B1"
	return []byte("Content-Type: multipart/report; boundary=\"" + boundary + "\"\r\n\r\n" +
		"--" + boundary + "\r\nContent-Type: text/plain\r\n\r\ntext\r\n\r\n" +
		"--" + boundary + "\r\nContent-Type: message/delivery-status\r\n\r\n" +
		"Reporting-MTA: dns; relay.example\r\n\r\n" +
		"Final-Recipient: rfc822;" + recipient + "\r\n" +
		"Action: failed\r\n" +
		"Status: 5.1.1\r\n" +
		"\r\n--" + boundary + "--\r\n")
}
