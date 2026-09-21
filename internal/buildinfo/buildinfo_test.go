package buildinfo

import "testing"

func TestGetFallbacks(t *testing.T) {
	oldV, oldC := Version, Commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })
	Version, Commit = "", ""
	if got := Get(); got.Version != "dev" || got.Commit != "unknown" {
		t.Fatalf("fallbacks = %+v", got)
	}
}

func TestGetInjectedValues(t *testing.T) {
	oldV, oldC := Version, Commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })
	Version, Commit = "v9.9.9", "abc1234"
	if got := Get(); got.Version != "v9.9.9" || got.Commit != "abc1234" {
		t.Fatalf("injected = %+v", got)
	}
}
