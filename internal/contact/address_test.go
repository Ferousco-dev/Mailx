package contact

import "testing"

func TestNormalizeBasic(t *testing.T) {
	got, err := Normalize(" <Alice@Example.COM.> ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Alice@example.com" {
		t.Fatalf("got %q, want local-part case preserved, domain lowercased, trailing dot stripped", got)
	}
}

func TestNormalizePreservesLocalPartCase(t *testing.T) {
	a, err1 := Normalize("Alice@example.com")
	b, err2 := Normalize("alice@example.com")
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if a == b {
		t.Fatal("local-part case must NOT be folded for contact identity")
	}
}

func TestNormalizeNoProviderFolding(t *testing.T) {
	a, _ := Normalize("john.smith@gmail.com")
	b, _ := Normalize("johnsmith@gmail.com")
	c, _ := Normalize("john+tag@gmail.com")
	if a == b || a == c {
		t.Fatal("dots/+tags must never be folded")
	}
}

func TestNormalizeRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "not-an-email", "a@", "@b.com", "a b@c.com", "a@127.0.0.1", "a@b..com", "Display <a@b.com>"} {
		if _, err := Normalize(in); err == nil {
			t.Fatalf("%q should be rejected", in)
		}
	}
}

func TestNormalizeRejectsControlCharsAndNonASCII(t *testing.T) {
	for _, in := range []string{"a\r\n@b.com", "a\x00@b.com", "üñíçødé@b.com"} {
		if _, err := Normalize(in); err == nil {
			t.Fatalf("%q should be rejected", in)
		}
	}
}

func TestNormalizeRejectsOversized(t *testing.T) {
	local := make([]byte, 65)
	for i := range local {
		local[i] = 'a'
	}
	if _, err := Normalize(string(local) + "@example.com"); err == nil {
		t.Fatal("local part over 64 bytes must be rejected")
	}
}
