package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/Ferousco-dev/mailx/internal/buildinfo"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// defaultObservabilityAddr is the operator listener (metrics + health). It is
// separate from the developer API listener; Compose publishes it on loopback.
const defaultObservabilityAddr = ":9090"

// obs bundles the process logger and metrics. Both are observers only.
type obs struct {
	log     *slog.Logger
	metrics *observability.Metrics
}

// newObs builds the logger from MAILX_LOG_LEVEL / MAILX_LOG_FORMAT (invalid
// values are a startup error) and the metrics registry.
func newObs() (obs, error) {
	logger, err := observability.NewLogger(os.Stderr, os.Getenv("MAILX_LOG_LEVEL"), os.Getenv("MAILX_LOG_FORMAT"))
	if err != nil {
		return obs{}, err
	}
	info := buildinfo.Get()
	m, err := observability.NewMetrics(info)
	if err != nil {
		return obs{}, err
	}
	logger = logger.With("version", info.Version, "commit", info.Commit)
	return obs{log: logger, metrics: m}, nil
}

// observabilityAddr returns the operator listen address. Unset selects the
// default; an explicitly empty value disables the operator listener.
func observabilityAddr() string {
	if v, ok := os.LookupEnv("MAILX_OBSERVABILITY_ADDR"); ok {
		return v
	}
	return defaultObservabilityAddr
}

// errLogger adapts a component's error callback to a structured record.
// Callback errors describe internal infrastructure failures (database,
// Redis, storage); delivery content and remote SMTP text never flow here.
func (o obs) errLogger(component string) func(error) {
	return func(err error) { o.log.Warn("component_error", "component", component, "error", err.Error()) }
}

// component wraps a component run function with start/stop records.
func (o obs) logged(name string, run func(context.Context) error) component {
	return component{name, func(ctx context.Context) error {
		o.log.Info("component_start", "component", name)
		err := run(ctx)
		if err != nil {
			o.log.Error("component_stop", "component", name, "result", "error", "error", err.Error())
		} else {
			o.log.Info("component_stop", "component", name, "result", "ok")
		}
		return err
	}}
}

// smtpConfig returns the default SMTP config with the observer attached.
func (o obs) smtpConfig() smtp.Config {
	cfg := smtp.DefaultConfig()
	cfg.Observer = observability.SMTPObserver{Logger: o.log, Metrics: o.metrics}
	return cfg
}

// smtpSink stores an accepted message and logs correlation IDs only: the
// session ID and the MailX message ID (never envelope, headers, or body).
func (o obs) smtpSink(store *storage.FileStore) func(smtp.Session, mail.Message) error {
	return func(s smtp.Session, m mail.Message) error {
		record, err := storage.NewMessageRecord(s.Envelope, m)
		if err == nil {
			err = store.Save(record)
		}
		if err != nil {
			o.log.Error("smtp_storage_failed", "session_id", s.ID)
			return err
		}
		o.log.Info("smtp_message_stored", "session_id", s.ID, "message_id", record.ID)
		return nil
	}
}

// readiness checks PostgreSQL and Redis; names are the bounded vocabulary
// exposed in health responses.
func readiness(db *database.DB, q *queue.RedisQueue) *observability.Readiness {
	return observability.NewReadiness(map[string]observability.Check{
		"postgres": db.Ping,
		"redis":    q.Ping,
	})
}
