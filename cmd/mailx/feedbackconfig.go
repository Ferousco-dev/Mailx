package main

import (
	"os"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/feedback"
)

// loadFeedbackConfig wires v0.32 outbound feedback ingestion. Both
// MAILX_FEEDBACK_INGEST_TOKEN and MAILX_FEEDBACK_CORRELATION_SECRET must be
// set together or the route stays disabled (nil): an ingestion endpoint with
// no credential, or a correlator with no secret, must never come up by
// accident.
func loadFeedbackConfig() (*api.FeedbackConfig, error) {
	token := os.Getenv("MAILX_FEEDBACK_INGEST_TOKEN")
	secret := os.Getenv("MAILX_FEEDBACK_CORRELATION_SECRET")
	if token == "" || secret == "" {
		return nil, nil
	}
	c, err := feedback.NewCorrelator([]byte(secret))
	if err != nil {
		return nil, err
	}
	return &api.FeedbackConfig{IngestToken: token, Correlator: c}, nil
}
