package retry

import (
	"testing"
	"time"
)

func TestJitterDisabledIsExactlyTheBackoff(t *testing.T) {
	p := BackoffPolicy{Base: 30 * time.Minute, Max: 4 * time.Hour}
	for seed := int64(0); seed < 50; seed++ {
		if got := p.Jitter(30*time.Minute, seed); got != 30*time.Minute {
			t.Fatalf("no jitter configured but got %s", got)
		}
	}
}

func TestJitterIsBoundedDeterministicAndSpreads(t *testing.T) {
	p := BackoffPolicy{Base: 30 * time.Minute, Max: 4 * time.Hour, JitterPercent: 10}
	base := 30 * time.Minute
	lo, hi := time.Duration(float64(base)*0.9), time.Duration(float64(base)*1.1)
	seen := map[time.Duration]bool{}
	for seed := int64(0); seed < 2000; seed++ {
		got := p.Jitter(base, seed)
		if got < lo || got > hi {
			t.Fatalf("seed %d: %s outside [%s,%s]", seed, got, lo, hi)
		}
		if got != p.Jitter(base, seed) {
			t.Fatal("jitter is not deterministic for a fixed seed")
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct delays across 2000 seeds: retries would still synchronize", len(seen))
	}
}

func TestJitterNeverExceedsMaxOrDropsBelowOneSecond(t *testing.T) {
	p := BackoffPolicy{Base: time.Hour, Max: 4 * time.Hour, JitterPercent: 50}
	for seed := int64(0); seed < 1000; seed++ {
		if got := p.Jitter(4*time.Hour, seed); got > 4*time.Hour {
			t.Fatalf("exceeds Max: %s", got)
		}
	}
	tiny := BackoffPolicy{Base: time.Second, Max: time.Minute, JitterPercent: 50}
	for seed := int64(0); seed < 1000; seed++ {
		if got := tiny.Jitter(time.Second, seed); got < time.Second {
			t.Fatalf("below one second: %s", got)
		}
	}
	// An out-of-range percent is clamped rather than trusted.
	wild := BackoffPolicy{Base: time.Hour, Max: 100 * time.Hour, JitterPercent: 900}
	for seed := int64(0); seed < 200; seed++ {
		if got := wild.Jitter(time.Hour, seed); got < 30*time.Minute || got > 90*time.Minute {
			t.Fatalf("unclamped percent produced %s", got)
		}
	}
}

// A retry storm: 500 messages that failed in the same second must not all come back at
// the same second. With 10% jitter on a 30 minute backoff they spread over a 6 minute window.
func TestJitterBreaksRetryStorms(t *testing.T) {
	p := BackoffPolicy{Base: 30 * time.Minute, Max: 4 * time.Hour, JitterPercent: 10}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	buckets := map[int64]int{}
	for i := 0; i < 500; i++ {
		now := start.Add(time.Duration(i) * 1700 * time.Microsecond) // all inside one second
		at := now.Add(p.Jitter(30*time.Minute, now.UnixNano()))
		buckets[at.Unix()]++
	}
	most := 0
	for _, n := range buckets {
		if n > most {
			most = n
		}
	}
	if len(buckets) < 100 || most > 20 {
		t.Fatalf("500 simultaneous failures retry in %d distinct seconds, worst second has %d", len(buckets), most)
	}
}

func TestJitterWithinTwentyPercent(t *testing.T) {
	p := BackoffPolicy{Base: time.Minute, Max: time.Hour, JitterPercent: 20}
	got := p.Jitter(time.Minute, 12345)
	if got < 48*time.Second || got > 72*time.Second {
		t.Fatalf("%s", got)
	}
}
