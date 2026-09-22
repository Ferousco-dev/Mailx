package bimi

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/webhook"
)

func testPolicy() webhook.URLPolicy {
	return webhook.URLPolicy{AllowHTTP: true, AllowPrivate: true}
}

func TestFetchRejectsNonHTTPS(t *testing.T) {
	f := NewAssetFetcher(testPolicy(), 1024, time.Second)
	_, err := f.Fetch(context.Background(), "http://example.com/logo.svg")
	if err != ErrAssetNotHTTPS {
		t.Fatalf("got %v", err)
	}
}

func TestFetchSucceedsWithinLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()
	policy := testPolicy()
	f := newFetcherForTestServer(t, srv, policy, 1024)
	body, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("got %q", body)
	}
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", 100)))
	}))
	defer srv.Close()
	policy := testPolicy()
	f := newFetcherForTestServer(t, srv, policy, 10)
	_, err := f.Fetch(context.Background(), srv.URL)
	if err != ErrAssetTooLarge {
		t.Fatalf("got %v", err)
	}
}

func TestFetchDoesNotFollowRedirects(t *testing.T) {
	var hitTarget bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hitTarget = true }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	policy := testPolicy()
	f := newFetcherForTestServer(t, source, policy, 1024)
	_, err := f.Fetch(context.Background(), source.URL)
	if err != ErrAssetFetch {
		t.Fatalf("expected a redirect response to be treated as a fetch failure, got %v", err)
	}
	if hitTarget {
		t.Fatal("the redirect target must never be followed")
	}
}

func TestFetchRejectsNon2xx(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	policy := testPolicy()
	f := newFetcherForTestServer(t, srv, policy, 1024)
	_, err := f.Fetch(context.Background(), srv.URL)
	if err != ErrAssetFetch {
		t.Fatalf("got %v", err)
	}
}

// newFetcherForTestServer builds an AssetFetcher whose transport trusts the
// httptest TLS server's self-signed certificate (matching the harness's own
// client, since AssetFetcher otherwise uses default TLS verification).
func newFetcherForTestServer(t *testing.T, srv *httptest.Server, policy webhook.URLPolicy, maxSize int64) *AssetFetcher {
	t.Helper()
	f := NewAssetFetcher(policy, maxSize, 2*time.Second)
	if tr, ok := f.client.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	}
	return f
}
