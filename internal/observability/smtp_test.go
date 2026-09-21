package observability

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestSMTPObserverLogsAndCountsWithoutContent(t *testing.T) {
	var buf bytes.Buffer
	l, _ := NewLogger(&buf, "debug", "json")
	m := newTestMetrics(t)
	o := SMTPObserver{Logger: l, Metrics: m}
	o.SessionStarted("sess_1")
	o.MessageResult("sess_1", "accepted", "")
	o.MessageResult("sess_1", "temporary_failure", "sink_failed")
	o.SessionEnded("sess_1", 5*time.Millisecond, false)
	o.SessionEnded("sess_2", time.Millisecond, true)
	o.SessionRejected()
	out := buf.String()
	for _, want := range []string{"smtp_session_start", "smtp_message", "smtp_session_end", "smtp_session_rejected", `"session_id":"sess_1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	gather(t, m)
	rec := scrape(m)
	for _, want := range []string{`mailx_smtp_messages_total{result="accepted"} 1`, `mailx_smtp_sessions_total{result="failed"} 1`,
		`mailx_smtp_sessions_total{result="rejected"} 1`, "mailx_smtp_active_sessions -1"} {
		if !strings.Contains(rec, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestZeroValueSMTPObserverIsSafe(t *testing.T) {
	SMTPObserver{}.SessionStarted("s")
	SMTPObserver{}.MessageResult("s", "accepted", "")
}
