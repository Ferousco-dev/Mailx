package api

import (
	"strings"
	"testing"
)

func trackingTestHandler() *emailHandler {
	return &emailHandler{trackingSecret: []byte("s3cret"), trackingBaseURL: "https://track.example.com"}
}

func TestInjectTrackingNoOpWithoutConfig(t *testing.T) {
	h := &emailHandler{}
	out, err := h.injectTracking("t1", "m1", "a@x.com", "<html><a href=\"https://x.com\">go</a></html>", true, true)
	if err != nil || out != "<html><a href=\"https://x.com\">go</a></html>" {
		t.Fatalf("expected no-op, got %q err=%v", out, err)
	}
}

func TestInjectTrackingClicksRewritesNonUnsubscribeLinks(t *testing.T) {
	h := trackingTestHandler()
	html := `<a href="https://example.com/a">a</a><a href="https://example.com/unsubscribe?x=1">unsub</a>`
	out, err := h.injectTracking("t1", "m1", "a@x.com", html, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "https://track.example.com/track/click/") {
		t.Fatalf("expected rewritten link: %s", out)
	}
	if !strings.Contains(out, `href="https://example.com/unsubscribe?x=1"`) {
		t.Fatalf("unsubscribe link must never be rewritten: %s", out)
	}
}

func TestInjectTrackingOpensAppendsPixel(t *testing.T) {
	h := trackingTestHandler()
	out, err := h.injectTracking("t1", "m1", "a@x.com", "<html><body>hi</body></html>", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "https://track.example.com/track/open/") {
		t.Fatalf("expected pixel injected: %s", out)
	}
}

func TestInjectTrackingEmptyHTMLNoOp(t *testing.T) {
	h := trackingTestHandler()
	out, err := h.injectTracking("t1", "m1", "a@x.com", "", true, true)
	if err != nil || out != "" {
		t.Fatalf("expected empty no-op, got %q err=%v", out, err)
	}
}
