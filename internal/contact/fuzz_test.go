package contact

import "testing"

func FuzzNormalize(f *testing.F) {
	f.Add("alice@example.com")
	f.Add(" <Alice@EXAMPLE.com.> ")
	f.Add("not-an-email")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked on %q: %v", s, r)
			}
		}()
		_, _ = Normalize(s)
	})
}
