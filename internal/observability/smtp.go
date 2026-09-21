package observability

import (
	"context"
	"log/slog"
	"time"
)

// SMTPObserver adapts smtp.Observer events to logs and metrics. It logs
// session IDs and bounded categories only — never addresses or content.
type SMTPObserver struct {
	Logger  *slog.Logger
	Metrics *Metrics
}

func (o SMTPObserver) log() *slog.Logger {
	if o.Logger == nil {
		return Discard()
	}
	return o.Logger
}

func (o SMTPObserver) SessionStarted(id string) {
	o.Metrics.SMTPSessionStarted()
	o.log().Debug("smtp_session_start", "session_id", id)
}

func (o SMTPObserver) SessionEnded(id string, d time.Duration, failed bool) {
	result := "completed"
	level := slog.LevelInfo
	if failed {
		result, level = "failed", slog.LevelWarn
	}
	o.Metrics.SMTPSessionEnded(result)
	o.log().Log(context.Background(), level, "smtp_session_end", "session_id", id, "result", result, "duration_ms", d.Milliseconds())
}

func (o SMTPObserver) SessionRejected() {
	o.Metrics.SMTPSessionRejected()
	o.log().Warn("smtp_session_rejected", "reason", "connection_limit")
}

func (o SMTPObserver) MessageResult(id, result, reason string) {
	o.Metrics.SMTPMessage(result)
	level := slog.LevelInfo
	if result != "accepted" {
		level = slog.LevelWarn
	}
	o.log().Log(context.Background(), level, "smtp_message", "session_id", id, "result", result, "reason", reason)
}
