package main

import (
	"strings"
	"testing"
)

func setRelay(t *testing.T, host, port, user, pass string) {
	t.Helper()
	t.Setenv("MAILX_RELAY_HOST", host)
	t.Setenv("MAILX_RELAY_PORT", port)
	t.Setenv("MAILX_RELAY_USERNAME", user)
	t.Setenv("MAILX_RELAY_PASSWORD", pass)
}

func TestRelayDisabledByDefault(t *testing.T) {
	setRelay(t, "", "", "", "")
	if r, err := outboundRelay(); r != nil || err != nil {
		t.Fatalf("%v %v", r, err)
	}
}

func TestRelayValidConfigurations(t *testing.T) {
	setRelay(t, "smtp.relay.example", "", "svc-user", "s3cret-value")
	r, err := outboundRelay()
	if err != nil || r.Host != "smtp.relay.example" || r.Port != 587 || r.Auth == nil {
		t.Fatalf("%+v %v", r, err)
	}
	setRelay(t, "10.1.2.3", "2525", "", "") // network-trusted relay, no AUTH
	if r, err := outboundRelay(); err != nil || r.Port != 2525 || r.Auth != nil {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRelayConfigurationErrorsNameSettingsNotValues(t *testing.T) {
	const secret = "TOP-SECRET-VALUE-XYZ"
	cases := map[string][4]string{
		"port without host":     {"", "587", "", ""},
		"user without host":     {"", "", "u", secret},
		"user without password": {"relay.example", "", "u", ""},
		"password without user": {"relay.example", "", "", secret},
		"bad port":              {"relay.example", "99999", "u", secret},
		"non-numeric port":      {"relay.example", "abc", "u", secret},
		"host with scheme":      {"smtp://relay.example", "", "u", secret},
		"host with space":       {"relay example", "", "u", secret},
		"control in password":   {"relay.example", "", "u", "a\nb" + secret},
		"long username":         {"relay.example", "", strings.Repeat("u", 300), secret},
	}
	for name, c := range cases {
		setRelay(t, c[0], c[1], c[2], c[3])
		r, err := outboundRelay()
		if err == nil || r != nil {
			t.Fatalf("%s must fail startup", name)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), c[2]) && c[2] != "" && len(c[2]) > 3 {
			t.Fatalf("%s: error leaks a value: %v", name, err)
		}
	}
}

func TestEngineConfigCarriesRelayOnlyWhenConfigured(t *testing.T) {
	if engineConfig(nil).Relay != nil {
		t.Fatal("no relay configured must mean direct delivery")
	}
}
