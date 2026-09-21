package webhook

import (
	"testing"
	"time"
)

func TestSignatureKnownVectorAndVerification(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	const timestamp = "1700000000"
	const want = "v1=af784f27423c462e20039559cd4264140f7b7ed4c9090e26fd663faa5eeb8dda"
	if got := Signature("secret", timestamp, body); got != want {
		t.Fatalf("signature = %s", got)
	}
	now := time.Unix(1700000000, 0)
	if err := VerifySignature("secret", timestamp, want, body, now, SignatureTolerance); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string][4]string{
		"modified body": {"secret", timestamp, want, `{"id":"evt_2"}`},
		"timestamp":     {"secret", "1700000001", want, string(body)},
		"wrong secret":  {"wrong", timestamp, want, string(body)},
		"malformed":     {"secret", timestamp, "bad", string(body)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := VerifySignature(tc[0], tc[1], tc[2], []byte(tc[3]), now, SignatureTolerance); err == nil {
				t.Fatal("verification unexpectedly succeeded")
			}
		})
	}
	if err := VerifySignature("secret", "1699999000", want, body, now, 5*time.Minute); err == nil {
		t.Fatal("expired timestamp accepted")
	}
}
