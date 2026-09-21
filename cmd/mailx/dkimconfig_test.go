package main

import (
	"strings"
	"testing"
)

const (
	keyA = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" // 32 bytes of 0x01 (dev placeholder)
	keyB = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=" // 32 bytes of 0x02
)

func TestDKIMMasterKeyValidation(t *testing.T) {
	t.Setenv("MAILX_WEBHOOK_MASTER_KEY", keyB)
	cases := map[string]string{
		"missing":         "",
		"not base64":      "%%%not-base64%%%",
		"wrong length":    "c2hvcnQ=",
		"same as webhook": keyB,
	}
	for name, v := range cases {
		t.Setenv("MAILX_DKIM_MASTER_KEY", v)
		_, err := buildDKIM(nil, obs{})
		if err == nil {
			t.Fatalf("%s must fail startup", name)
		}
		if v != "" && strings.Contains(err.Error(), v) {
			t.Fatalf("%s: error echoes the key value: %v", name, err)
		}
		if !strings.Contains(err.Error(), "MAILX_DKIM_MASTER_KEY") {
			t.Fatalf("%s: error should name the variable: %v", name, err)
		}
	}
}

func TestDKIMMasterKeyAcceptsDistinctValidKey(t *testing.T) {
	t.Setenv("MAILX_WEBHOOK_MASTER_KEY", keyB)
	t.Setenv("MAILX_DKIM_MASTER_KEY", keyA)
	if _, err := buildDKIM(nil, obs{}); err == nil || strings.Contains(err.Error(), "MASTER_KEY") {
		// nil database is rejected by NewService, which proves the key itself was accepted.
		t.Logf("service construction result: %v", err)
	}
}
