package auth

import (
	"strings"
	"testing"
)

func TestGenerateFormatAndEntropy(t *testing.T) {
	gen, err := Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gen.Raw, "mx_") {
		t.Fatalf("expected mx_ prefix, got %q", gen.Raw)
	}
	if len(gen.KeyID) != keyIDHexLen {
		t.Fatalf("expected key id of %d hex chars, got %d", keyIDHexLen, len(gen.KeyID))
	}
	parts := strings.Split(gen.Raw, "_")
	if len(parts) != 3 || len(parts[2]) != secretHexLen {
		t.Fatalf("expected secret of %d hex chars, got parts=%v", secretHexLen, parts)
	}
	if strings.Contains(gen.SecretHash, parts[2]) {
		t.Fatal("stored hash must not contain the raw secret")
	}
}

func TestGenerateUniqueness(t *testing.T) {
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		gen, err := Generate(nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[gen.Raw] {
			t.Fatalf("duplicate key generated at iteration %d: %s", i, gen.Raw)
		}
		seen[gen.Raw] = true
	}
}

func TestParseRoundTrip(t *testing.T) {
	gen, err := Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyID, secret, err := Parse(gen.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != gen.KeyID {
		t.Fatalf("keyID mismatch: got %q want %q", keyID, gen.KeyID)
	}
	if Hash(secret, nil) != gen.SecretHash {
		t.Fatal("parsed secret does not hash back to the stored verifier")
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	cases := []string{
		"",
		"mx_",
		"mx_onlyonepart",
		"notmx_" + strings.Repeat("a", keyIDHexLen) + "_" + strings.Repeat("b", secretHexLen),
		"mx_" + strings.Repeat("a", keyIDHexLen-1) + "_" + strings.Repeat("b", secretHexLen),
		"mx_" + strings.Repeat("g", keyIDHexLen) + "_" + strings.Repeat("b", secretHexLen), // non-hex
		"mx_" + strings.Repeat("a", keyIDHexLen) + "_" + strings.Repeat("b", secretHexLen) + "_extra",
		strings.Repeat("x", 10000), // oversized
		"mx__",
		"mx_" + strings.Repeat("a", keyIDHexLen) + "_",
	}
	for _, c := range cases {
		if _, _, err := Parse(c); err == nil {
			t.Errorf("expected Parse(%q) to fail", c)
		}
	}
}

func FuzzParse(f *testing.F) {
	gen, _ := Generate(nil)
	f.Add(gen.Raw)
	f.Add("")
	f.Add("mx_")
	f.Add("mx___")
	f.Add(strings.Repeat("mx_", 100))
	f.Fuzz(func(t *testing.T, raw string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Parse panicked on input %q: %v", raw, r)
			}
		}()
		_, _, _ = Parse(raw)
	})
}

func TestHashDeterministicAndPepperChangesResult(t *testing.T) {
	secret := "abc123"
	h1 := Hash(secret, nil)
	h2 := Hash(secret, nil)
	if h1 != h2 {
		t.Fatal("Hash must be deterministic for the same input")
	}
	h3 := Hash(secret, []byte("pepper"))
	if h1 == h3 {
		t.Fatal("a pepper must change the resulting hash")
	}
}

func TestVerify(t *testing.T) {
	secret := "the-secret"
	pepper := []byte("server-pepper")
	hash := Hash(secret, pepper)

	if !Verify(secret, hash, pepper) {
		t.Fatal("expected verification to succeed with the correct secret and pepper")
	}
	if Verify("wrong-secret", hash, pepper) {
		t.Fatal("expected verification to fail with the wrong secret")
	}
	if Verify(secret, hash, []byte("different-pepper")) {
		t.Fatal("expected verification to fail with the wrong pepper")
	}
	if Verify(secret, hash, nil) {
		t.Fatal("expected verification to fail when pepper is dropped entirely")
	}
}

func TestValidScope(t *testing.T) {
	if !ValidScope("emails:send") || !ValidScope("emails:read") {
		t.Fatal("expected the two current scopes to be valid")
	}
	if ValidScope("emails:delete") || ValidScope("") || ValidScope("domains:verify") {
		t.Fatal("expected unrecognized scopes to be rejected")
	}
}
