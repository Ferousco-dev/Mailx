package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/ratelimit"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAbuseDefaultsAreValidAndOn(t *testing.T) {
	p, enabled, err := loadAbusePolicy(env(nil))
	if err != nil || !enabled {
		t.Fatalf("%v %v", enabled, err)
	}
	if p != ratelimit.DefaultPolicy() {
		t.Fatalf("defaults drifted: %+v", p)
	}
}

func TestAbuseOverridesApply(t *testing.T) {
	p, _, err := loadAbusePolicy(env(map[string]string{
		"MAILX_LIMIT_TENANT_RPS": "5.5", "MAILX_LIMIT_TENANT_BURST": "40", "MAILX_LIMIT_KEY_RPS": "2", "MAILX_LIMIT_KEY_BURST": "10",
		"MAILX_LIMIT_MAX_RECIPIENTS": "10", "MAILX_LIMIT_RECIPIENTS_BURST": "100", "MAILX_LIMIT_PERMIT_TTL": "30m",
		"MAILX_RETRY_JITTER_PERCENT": "0", "MAILX_LIMIT_TENANT_CONCURRENCY": "3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantRequestRate != 5.5 || p.TenantRequestBurst != 40 || p.MaxRecipientsPerMessage != 10 || p.PermitTTL != 30*time.Minute ||
		p.RetryJitterPercent != 0 || p.TenantDeliveryConcurrency != 3 {
		t.Fatalf("%+v", p)
	}
}

// Invalid values fail startup and name the variable; nothing is silently defaulted.
func TestAbuseInvalidValuesFailAndNameTheVariable(t *testing.T) {
	cases := map[string]map[string]string{
		"MAILX_LIMIT_TENANT_RPS":           {"MAILX_LIMIT_TENANT_RPS": "fast"},
		"MAILX_LIMIT_TENANT_BURST":         {"MAILX_LIMIT_TENANT_BURST": "1.5"},
		"MAILX_LIMIT_PERMIT_TTL":           {"MAILX_LIMIT_PERMIT_TTL": "forever"},
		"MAILX_ABUSE_CONTROLS":             {"MAILX_ABUSE_CONTROLS": "maybe"},
		"MAILX_LIMIT_KEY_RPS":              {"MAILX_LIMIT_KEY_RPS": "NaN"},
		"tenant request rate":              {"MAILX_LIMIT_TENANT_RPS": "0"},
		"tenant request burst":             {"MAILX_LIMIT_TENANT_BURST": "-3"},
		"API key request limit":            {"MAILX_LIMIT_KEY_BURST": "9999", "MAILX_LIMIT_TENANT_BURST": "10"},
		"tenant recipient burst":           {"MAILX_LIMIT_MAX_RECIPIENTS": "600"},
		"permit TTL must be":               {"MAILX_LIMIT_PERMIT_TTL": "1s"},
		"retry jitter percent":             {"MAILX_RETRY_JITTER_PERCENT": "80"},
		"tenant max queued messages":       {"MAILX_LIMIT_TENANT_MAX_QUEUED": "0"},
		"tenant delivery concurrency":      {"MAILX_LIMIT_TENANT_CONCURRENCY": "0"},
		"destination delivery concurrency": {"MAILX_LIMIT_DESTINATION_CONCURRENCY": "100001"},
		"MAILX_LIMIT_MAX_PENDING_DISPATCH": {"MAILX_LIMIT_MAX_PENDING_DISPATCH": "lots"},
	}
	for want, m := range cases {
		_, enabled, err := loadAbusePolicy(env(m))
		if err == nil || enabled {
			t.Errorf("%v accepted", m)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v: error %q should mention %q", m, err, want)
		}
	}
}

func TestAbuseOptOutIsExplicitOnly(t *testing.T) {
	for _, v := range []string{"off", "OFF", " off "} {
		if _, enabled, err := loadAbusePolicy(env(map[string]string{"MAILX_ABUSE_CONTROLS": v})); err != nil || enabled {
			t.Fatalf("%q: enabled=%v err=%v", v, enabled, err)
		}
	}
	for _, v := range []string{"", "on", "ON"} {
		if _, enabled, err := loadAbusePolicy(env(map[string]string{"MAILX_ABUSE_CONTROLS": v})); err != nil || !enabled {
			t.Fatalf("%q: enabled=%v err=%v", v, enabled, err)
		}
	}
	// Anything that is not exactly on/off (0, false, disabled, no) is rejected, never treated as off.
	for _, v := range []string{"0", "false", "no", "disabled", "of"} {
		if _, enabled, err := loadAbusePolicy(env(map[string]string{"MAILX_ABUSE_CONTROLS": v})); err == nil || enabled {
			t.Fatalf("%q must not silently disable controls", v)
		}
	}
}

// Whatever the environment says, a policy the loader accepts also passes Validate, and the
// loader never panics.
func FuzzLoadAbusePolicy(f *testing.F) {
	f.Add("50", "100", "10m", "10", "on")
	f.Add("NaN", "-1", "0", "999", "off")
	f.Add("1e309", "9223372036854775808", "24h1s", "", "")
	f.Fuzz(func(t *testing.T, rps, burst, ttl, jitter, mode string) {
		p, enabled, err := loadAbusePolicy(env(map[string]string{
			"MAILX_LIMIT_TENANT_RPS": rps, "MAILX_LIMIT_TENANT_BURST": burst, "MAILX_LIMIT_PERMIT_TTL": ttl,
			"MAILX_RETRY_JITTER_PERCENT": jitter, "MAILX_ABUSE_CONTROLS": mode,
		}))
		if err == nil && enabled {
			if verr := p.Validate(); verr != nil {
				t.Fatalf("accepted a policy that fails Validate: %v (%+v)", verr, p)
			}
		}
	})
}
