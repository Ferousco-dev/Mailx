package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
)

func TestBroadcastControlHTTPWorkflow(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "controls")
	first := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	rec := doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(map[string]any{"send_at": first}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	b := decodeBroadcast(t, rec)
	second := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	rec = doJSON(t, f.h, "POST", "/v1/broadcasts/"+b.ID+"/reschedule", map[string]any{"send_at": second})
	if rec.Code != http.StatusOK {
		t.Fatalf("reschedule: %d %s", rec.Code, rec.Body.String())
	}
	changed := decodeBroadcast(t, rec)
	if changed.Status != "accepted" || changed.SendAt == nil || changed.SendAt.UTC().Format(time.RFC3339) != second {
		t.Fatalf("rescheduled resource: %+v", changed)
	}
	for i := 0; i < 2; i++ {
		rec = doJSON(t, f.h, "POST", "/v1/broadcasts/"+b.ID+"/cancel", nil)
		if rec.Code != http.StatusOK || decodeBroadcast(t, rec).Status != "cancelled" {
			t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
		}
	}
	rec = doJSON(t, f.h, "GET", "/v1/broadcasts/"+b.ID, nil)
	if rec.Code != http.StatusOK || decodeBroadcast(t, rec).Status != "cancelled" {
		t.Fatalf("persisted cancellation: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, f.h, "POST", "/v1/broadcasts/"+b.ID+"/reschedule", map[string]any{"send_at": second})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "broadcast_state_conflict") {
		t.Fatalf("rescheduled cancellation: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastControlHTTPAuthenticationAndIsolation(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "owner")
	b := decodeBroadcast(t, doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(nil)))
	foreign, _ := a.scopedActor("foreign", auth.ScopeBroadcastsWrite)
	readOnly, _ := a.scopedActor("reader", auth.ScopeBroadcastsRead)
	for _, action := range []string{"cancel", "reschedule"} {
		path := "/v1/broadcasts/" + b.ID + "/" + action
		body := map[string]any{"send_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
		for _, c := range []struct {
			h    http.Handler
			want int
		}{{a.mux, http.StatusUnauthorized}, {foreign.h, http.StatusNotFound}, {readOnly.h, http.StatusForbidden}} {
			rec := doJSON(t, c.h, "POST", path, body)
			if rec.Code != c.want {
				t.Fatalf("%s: want %d, got %d %s", action, c.want, rec.Code, rec.Body.String())
			}
		}
	}
	if got := decodeBroadcast(t, doJSON(t, f.h, "GET", "/v1/broadcasts/"+b.ID, nil)); got.Status != "accepted" {
		t.Fatalf("foreign request changed broadcast: %+v", got)
	}
}

func TestBroadcastRescheduleHTTPRejectsInvalidBodies(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "validation")
	b := decodeBroadcast(t, doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(nil)))
	path := "/v1/broadcasts/" + b.ID + "/reschedule"
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{}`, 422}, {`null`, 422}, {`{"send_at":null}`, 422},
		{`{"send_at":"yesterday"}`, 422}, {`{"send_at":"2000-01-01T00:00:00Z"}`, 422},
		{`{"send_at":5}`, 400}, {`{"unknown":true}`, 400}, {`{`, 400},
		{`{"send_at":"` + future + `"} {}`, 400},
	} {
		r := httptest.NewRequest("POST", path, strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Fatalf("%s: want %d, got %d %s", c.body, c.want, rec.Code, rec.Body.String())
		}
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastRescheduleHTTPConflictsOnceExpanding(t *testing.T) {
	a := newDKIMAPI(t)
	f := setupBroadcastReady(t, a, "active")
	b := decodeBroadcast(t, doJSON(t, f.h, "POST", "/v1/broadcasts", f.body(nil)))
	if err := a.db.MarkBroadcastExpanding(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, f.h, "POST", "/v1/broadcasts/"+b.ID+"/reschedule", map[string]any{"send_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	if rec.Code != http.StatusConflict {
		t.Fatalf("active reschedule: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, f.h, "POST", "/v1/broadcasts/"+b.ID+"/cancel", nil)
	if rec.Code != http.StatusOK || decodeBroadcast(t, rec).Status != "cancelled" {
		t.Fatalf("active cancel: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBroadcastControlOpenAPIContract(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(openAPISpec), &doc); err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	for _, action := range []string{"cancel", "reschedule"} {
		path := "/broadcasts/{id}/" + action
		item, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("missing path %s", path)
		}
		op := item["post"].(map[string]any)
		for _, status := range []string{"200", "401", "403", "404", "409", "429", "500"} {
			if _, ok := op["responses"].(map[string]any)[status]; !ok {
				t.Errorf("%s missing response %s", path, status)
			}
		}
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, name := range []string{"Broadcast", "BroadcastRecipient"} {
		status := schemas[name].(map[string]any)["properties"].(map[string]any)["status"].(map[string]any)
		found := false
		for _, v := range status["enum"].([]any) {
			found = found || v == "cancelled"
		}
		if !found {
			t.Errorf("%s missing cancelled status", name)
		}
	}
}
