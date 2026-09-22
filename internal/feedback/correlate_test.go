package feedback

import (
	"strings"
	"testing"
)

func testSecret() []byte { return []byte("0123456789abcdef0123456789abcdef") }

func TestCorrelatorRoundTrip(t *testing.T) {
	c, err := NewCorrelator(testSecret())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Token("msg123")
	if err != nil {
		t.Fatal(err)
	}
	id, ok := c.Verify(tok)
	if !ok || id != "msg123" {
		t.Fatalf("id=%q ok=%v", id, ok)
	}
}

func TestCorrelatorRejectsShortSecret(t *testing.T) {
	if _, err := NewCorrelator([]byte("short")); err != ErrShortSecret {
		t.Fatalf("%v", err)
	}
}

func TestCorrelatorRejectsTamperedToken(t *testing.T) {
	c, _ := NewCorrelator(testSecret())
	tok, _ := c.Token("victim-message")
	// Flip the last hex character of the MAC.
	tampered := tok[:len(tok)-1] + flip(tok[len(tok)-1])
	if _, ok := c.Verify(tampered); ok {
		t.Fatal("tampered token verified")
	}
}

func flip(b byte) string {
	if b == '0' {
		return "1"
	}
	return "0"
}

func TestCorrelatorRejectsForgedMessageID(t *testing.T) {
	c, _ := NewCorrelator(testSecret())
	// An attacker who knows (or guesses) a real message ID still cannot produce
	// a valid token without the secret.
	if _, ok := c.Verify("victim-message.deadbeefdeadbeefdeadbeefdeadbeef"); ok {
		t.Fatal("forged token verified")
	}
}

func TestCorrelatorRejectsWrongSecret(t *testing.T) {
	c1, _ := NewCorrelator(testSecret())
	c2, _ := NewCorrelator([]byte("ffffffffffffffffffffffffffffffff"))
	tok, _ := c1.Token("m")
	if _, ok := c2.Verify(tok); ok {
		t.Fatal("verified under the wrong secret")
	}
}

func TestCorrelatorRejectsGarbageTokens(t *testing.T) {
	c, _ := NewCorrelator(testSecret())
	for _, bad := range []string{"", ".", "novaluehere", "id.", ".mac", "id.nothex!!", strings.Repeat("a", 10000)} {
		if _, ok := c.Verify(bad); ok {
			t.Fatalf("%q verified", bad)
		}
	}
}

// Replay: verifying the same valid token twice must succeed both times (Verify
// is stateless — replay protection, if needed, is the caller's job via the
// feedback dedup key, not the token itself).
func TestCorrelatorTokenIsReplayableByDesign(t *testing.T) {
	c, _ := NewCorrelator(testSecret())
	tok, _ := c.Token("m")
	for i := 0; i < 3; i++ {
		if _, ok := c.Verify(tok); !ok {
			t.Fatalf("replay %d failed", i)
		}
	}
}

func TestCorrelatorTokenDoesNotLeakMessageIDShape(t *testing.T) {
	c, _ := NewCorrelator(testSecret())
	tok, _ := c.Token("abc")
	if !strings.HasPrefix(tok, "abc.") {
		t.Fatalf("token %q should embed the id verbatim as a prefix (opaque, not encrypted, but reveals nothing beyond the id itself)", tok)
	}
}
