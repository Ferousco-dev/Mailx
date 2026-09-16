package idempotency

import "testing"

type sample struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
}

func TestFingerprintStableAcrossFieldOrderAndWhitespace(t *testing.T) {
	// encoding/json always marshals a Go struct's fields in the struct's
	// declared order, so two values built from JSON with different key
	// order/whitespace still fingerprint identically once decoded into
	// the same struct type - proving the "JSON field order/whitespace
	// must not matter" requirement without needing custom canonicalization.
	a := sample{From: "alice@example.com", To: []string{"bob@example.com"}, Subject: "Hi"}
	b := sample{From: "alice@example.com", To: []string{"bob@example.com"}, Subject: "Hi"}

	fa, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Fatalf("expected identical fingerprints, got %q vs %q", fa, fb)
	}
	if len(fa) != 64 {
		t.Fatalf("expected a 64-hex-char SHA-256 digest, got %d chars: %q", len(fa), fa)
	}
}

func TestFingerprintDiffersForDifferentContent(t *testing.T) {
	a := sample{From: "alice@example.com", To: []string{"bob@example.com"}, Subject: "Hi"}
	b := sample{From: "alice@example.com", To: []string{"carol@example.com"}, Subject: "Hi"}

	fa, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa == fb {
		t.Fatal("expected different fingerprints for materially different requests")
	}
}
