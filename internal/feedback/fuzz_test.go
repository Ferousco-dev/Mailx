package feedback

import "testing"

// FuzzParseDSN feeds arbitrary bytes to the parser: it must never panic and
// never allocate unboundedly (the corpus seeds a valid DSN so the fuzzer can
// mutate real structure, not just hit ErrNotDSN immediately every time).
func FuzzParseDSN(f *testing.F) {
	f.Add(dsnMessage(nil, "human text", permanentDeliveryStatus()))
	f.Add(dsnMessage(nil, "delayed", "Reporting-MTA: dns; r\r\n\r\nFinal-Recipient: rfc822;a@b.com\r\nAction: delayed\r\nStatus: 4.4.7\r\n"))
	f.Add([]byte(""))
	f.Add([]byte("not a dsn at all"))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ParseDSN panicked on %q: %v", data, r)
			}
		}()
		groups, err := ParseDSN(data)
		if err == nil {
			for _, g := range groups {
				_, _ = Classify(g) // must also never panic
			}
		}
	})
}

func FuzzCorrelatorVerify(f *testing.F) {
	c, _ := NewCorrelator(testSecret())
	tok, _ := c.Token("some-message-id")
	f.Add(tok)
	f.Add("")
	f.Add(".")
	f.Fuzz(func(t *testing.T, token string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Verify panicked on %q: %v", token, r)
			}
		}()
		_, _ = c.Verify(token)
	})
}
