package mailx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendEmailSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/emails" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing auth header")
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Email{ID: "em_1", Status: "queued"})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	email, err := c.SendEmail(context.Background(), SendEmailRequest{From: "a@b.com", To: []string{"c@d.com"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email.ID != "em_1" {
		t.Fatalf("unexpected id: %s", email.ID)
	}
}

func TestRetriesOn429ThenSucceeds(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "rate_limited", "code": "too_many_requests", "message": "slow down"}})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Email{ID: "em_2"})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	email, err := c.SendEmail(context.Background(), SendEmailRequest{From: "a@b.com", To: []string{"c@d.com"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if email.ID != "em_2" {
		t.Fatalf("unexpected id: %s", email.ID)
	}
}

func TestNonRetryableErrorReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "invalid_request", "code": "missing_field", "message": "from is required"}})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	_, err := c.SendEmail(context.Background(), SendEmailRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Status != 400 || apiErr.Code != "missing_field" {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
}

func TestGetEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/emails/em_3" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(Email{ID: "em_3", Status: "delivered"})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	email, err := c.GetEmail(context.Background(), "em_3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email.Status != "delivered" {
		t.Fatalf("unexpected status: %s", email.Status)
	}
}
