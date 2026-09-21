package main

import (
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/spf"
)

func setSPF(t *testing.T, relayHost, ips, include string) {
	t.Helper()
	t.Setenv("MAILX_RELAY_HOST", relayHost)
	t.Setenv("MAILX_RELAY_PORT", "")
	t.Setenv("MAILX_RELAY_USERNAME", "")
	t.Setenv("MAILX_RELAY_PASSWORD", "")
	t.Setenv("MAILX_SENDING_IPS", ips)
	t.Setenv("MAILX_SPF_RELAY_INCLUDE", include)
}

func TestSPFConfigDefaultsLeaveLocalDevelopmentUsable(t *testing.T) {
	setSPF(t, "", "", "")
	cfg, err := spfConfig(false)
	if err != nil || cfg.Mode != spf.ModeDirect || cfg.Ready() {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestSPFConfigDirectAndRelay(t *testing.T) {
	setSPF(t, "", "8.8.8.8, 2606:4700:4700::1111", "")
	cfg, err := spfConfig(false)
	if err != nil || !cfg.Ready() || len(cfg.IPs) != 2 {
		t.Fatalf("%+v %v", cfg, err)
	}
	setSPF(t, "smtp.relay.example", "", "_SPF.Relay.Example.")
	cfg, err = spfConfig(true)
	if err != nil || cfg.Mode != spf.ModeRelay || cfg.RelayInclude != "_spf.relay.example" {
		t.Fatalf("%+v %v", cfg, err)
	}
	setSPF(t, "smtp.relay.example", "", "")
	if cfg, err = spfConfig(true); err != nil || cfg.Ready() {
		t.Fatalf("relay without include must be 'unknown', not invented: %+v %v", cfg, err)
	}
}

func TestSPFConfigRejections(t *testing.T) {
	for name, tc := range map[string]struct {
		relay           bool
		ips, include    string
		mustNameSetting string
	}{
		"loopback":           {false, "127.0.0.1", "", "MAILX_SENDING_IPS"},
		"private":            {false, "10.0.0.5", "", "MAILX_SENDING_IPS"},
		"documentation":      {false, "203.0.113.10", "", "MAILX_SENDING_IPS"},
		"ipv6 loopback":      {false, "::1", "", "MAILX_SENDING_IPS"},
		"garbage":            {false, "not-an-ip", "", "MAILX_SENDING_IPS"},
		"cidr":               {false, "8.8.8.0/24", "", "MAILX_SENDING_IPS"},
		"ips in relay mode":  {true, "8.8.8.8", "", "MAILX_SENDING_IPS"},
		"include in direct":  {false, "", "_spf.relay.example", "MAILX_SPF_RELAY_INCLUDE"},
		"include is an IP":   {true, "", "8.8.8.8", "MAILX_SPF_RELAY_INCLUDE"},
		"include with space": {true, "", "a b.example", "MAILX_SPF_RELAY_INCLUDE"},
	} {
		setSPF(t, "", tc.ips, tc.include)
		_, err := spfConfig(tc.relay)
		if err == nil || !strings.Contains(err.Error(), tc.mustNameSetting) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
