package tracking

import "testing"

func TestSignVerifyRoundTrip(t *testing.T) {
	secret := []byte("s3cret")
	p := Payload{TenantID: "t1", MessageID: "m1", Recipient: "a@example.com", URL: "https://example.com/x"}
	tok, err := Sign(secret, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("got %+v want %+v", got, p)
	}
}

func TestVerifyRejectsTamperedToken(t *testing.T) {
	secret := []byte("s3cret")
	tok, _ := Sign(secret, Payload{TenantID: "t1", MessageID: "m1", Recipient: "a@example.com"})
	if _, err := Verify(secret, tok+"x"); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	tok, _ := Sign([]byte("s1"), Payload{TenantID: "t1", MessageID: "m1", Recipient: "a@example.com"})
	if _, err := Verify([]byte("s2"), tok); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "noseparator", ".", "a.", ".b"} {
		if _, err := Verify([]byte("s"), bad); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
}
