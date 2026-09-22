package main

import (
	"fmt"
	"os"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/feedback"
)

// loadFeedbackConfig wires v0.32 outbound feedback ingestion. Both
// MAILX_FEEDBACK_INGEST_TOKEN and MAILX_FEEDBACK_CORRELATION_SECRET must be
// set together (route enabled) or both unset (route disabled, nil) — an
// ingestion endpoint with no credential, or a correlator with no secret,
// must never come up by accident. PR review fix: setting only ONE of the
// two previously started MailX successfully with ingestion silently
// disabled — no startup signal — so a deployment typo could leave complaint/
// hard-bounce suppression inactive while the service reported healthy. That
// is now a fatal startup error instead of a silent no-op.
func loadFeedbackConfig() (*api.FeedbackConfig, error) {
	token := os.Getenv("MAILX_FEEDBACK_INGEST_TOKEN")
	secret := os.Getenv("MAILX_FEEDBACK_CORRELATION_SECRET")
	if token == "" && secret == "" {
		return nil, nil
	}
	if token == "" || secret == "" {
		return nil, fmt.Errorf("MAILX_FEEDBACK_INGEST_TOKEN and MAILX_FEEDBACK_CORRELATION_SECRET must both be set (or both unset) — only one is set")
	}
	c, err := feedback.NewCorrelator([]byte(secret))
	if err != nil {
		return nil, err
	}
	return &api.FeedbackConfig{IngestToken: token, Correlator: c}, nil
}
