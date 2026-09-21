package webhook

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func testClaim(rawURL string, attempt int) database.ClaimedWebhookDelivery {
	return database.ClaimedWebhookDelivery{
		WebhookDelivery: database.WebhookDelivery{ID: "delivery-1", AttemptCount: attempt},
		URL:             rawURL,
		Event: database.Event{ID: "event-1", MessageID: "email-1", Type: database.EventDelivered,
			OccurredAt: time.Unix(1700000000, 0).UTC(), Metadata: map[string]any{}},
	}
}

func devClient() *Client {
	return NewClient(URLPolicy{AllowHTTP: true, AllowPrivate: true})
}

func TestBuildRequestSignsExactSentBytes(t *testing.T) {
	client := devClient()
	client.now = func() time.Time { return time.Unix(1700000000, 0) }
	req, serialized, err := client.BuildRequest(context.Background(), testClaim("http://127.0.0.1/hook", 1), "secret")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(sent) != string(serialized) {
		t.Fatal("signed bytes differ from request body bytes")
	}
	if err := VerifySignature("secret", req.Header.Get("MailX-Webhook-Timestamp"),
		req.Header.Get("MailX-Webhook-Signature"), sent, client.now(), SignatureTolerance); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("MailX-Event-Id") != "event-1" || !strings.Contains(string(sent), `"id":"event-1"`) {
		t.Fatal("stable event id missing from header or payload")
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		code       int
		wantStatus string
	}{
		{200, "succeeded"}, {201, "succeeded"}, {204, "succeeded"},
		{302, "failed"}, {400, "failed"}, {404, "failed"},
		{408, "retrying"}, {429, "retrying"}, {500, "retrying"}, {503, "retrying"},
	} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.code) }))
			defer server.Close()
			got := devClient().Deliver(context.Background(), testClaim(server.URL, 1), "secret")
			if got.Status != tc.wantStatus || got.ResponseCode == nil || *got.ResponseCode != tc.code {
				t.Fatalf("outcome = %+v", got)
			}
		})
	}
}

func TestRedirectIsNeverFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer source.Close()
	outcome := devClient().Deliver(context.Background(), testClaim(source.URL, 1), "secret")
	if outcome.Status != "failed" || targetHits.Load() != 0 {
		t.Fatalf("redirect followed or misclassified: %+v hits=%d", outcome, targetHits.Load())
	}
}

func TestNetworkTimeoutTLSAndHugeResponseAreBounded(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(100 * time.Millisecond) }))
		defer server.Close()
		client := devClient()
		client.httpClient.Timeout = 20 * time.Millisecond
		if got := client.Deliver(context.Background(), testClaim(server.URL, 1), "secret"); got.Status != "retrying" {
			t.Fatalf("timeout = %+v", got)
		}
	})
	t.Run("tls", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()
		if got := devClient().Deliver(context.Background(), testClaim(server.URL, 1), "secret"); got.Status != "retrying" {
			t.Fatalf("TLS failure = %+v", got)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		if got := devClient().Deliver(context.Background(), testClaim("http://"+address, 1), "secret"); got.Status != "retrying" {
			t.Fatalf("connection refusal = %+v", got)
		}
	})
	t.Run("connection reset", func(t *testing.T) {
		address, closeServer := rawHTTPServer(t, func(conn net.Conn) { _ = conn.Close() })
		defer closeServer()
		if got := devClient().Deliver(context.Background(), testClaim("http://"+address, 1), "secret"); got.Status != "retrying" {
			t.Fatalf("connection reset = %+v", got)
		}
	})
	t.Run("malformed response", func(t *testing.T) {
		address, closeServer := rawHTTPServer(t, func(conn net.Conn) {
			defer conn.Close()
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = io.WriteString(conn, "this is not HTTP\r\n\r\n")
		})
		defer closeServer()
		if got := devClient().Deliver(context.Background(), testClaim("http://"+address, 1), "secret"); got.Status != "retrying" {
			t.Fatalf("malformed response = %+v", got)
		}
	})
	t.Run("huge response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
		}))
		defer server.Close()
		if got := devClient().Deliver(context.Background(), testClaim(server.URL, 1), "secret"); got.Status != "retrying" {
			t.Fatalf("huge response = %+v", got)
		}
	})
}

func rawHTTPServer(t *testing.T, handler func(net.Conn)) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			handler(conn)
		}
	}()
	return listener.Addr().String(), func() {
		_ = listener.Close()
		<-done
	}
}

func TestRetryPolicyBoundsAndExhaustion(t *testing.T) {
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		minDelay := min(time.Minute<<min(attempt-1, 6), time.Hour) / 2
		maxDelay := min(time.Minute<<min(attempt-1, 6), time.Hour)
		for _, entropy := range []uint64{0, ^uint64(0)} {
			got := retryDelay(attempt, entropy)
			if got < minDelay || got > maxDelay {
				t.Fatalf("attempt %d delay %s outside [%s,%s]", attempt, got, minDelay, maxDelay)
			}
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	if got := devClient().Deliver(context.Background(), testClaim(server.URL, MaxAttempts), "secret"); got.Status != "failed" || got.NextRetryAt != nil {
		t.Fatalf("max-attempt outcome = %+v", got)
	}
}

func TestRetryAfterIsHonoredAndCapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := devClient()
	now := time.Unix(1700000000, 0).UTC()
	client.now = func() time.Time { return now }
	got := client.Deliver(context.Background(), testClaim(server.URL, 1), "secret")
	if got.NextRetryAt == nil || !got.NextRetryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("Retry-After not capped: %+v", got)
	}
}
