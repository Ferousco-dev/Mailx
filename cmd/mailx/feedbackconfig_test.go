package main

import "testing"

func TestFeedbackConfigDisabledWhenBothUnset(t *testing.T) {
	c, err := loadFeedbackConfig()
	if err != nil || c != nil {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestFeedbackConfigEnabledWhenBothSet(t *testing.T) {
	t.Setenv("MAILX_FEEDBACK_INGEST_TOKEN", "tok")
	t.Setenv("MAILX_FEEDBACK_CORRELATION_SECRET", "at-least-32-bytes-long-correlation-secret")
	c, err := loadFeedbackConfig()
	if err != nil || c == nil || c.IngestToken != "tok" {
		t.Fatalf("%+v %v", c, err)
	}
}

// PR review fix: setting only one of the two required variables must be a
// fatal startup error, not a silent "ingestion disabled".
func TestFeedbackConfigPartialSetIsError(t *testing.T) {
	t.Setenv("MAILX_FEEDBACK_INGEST_TOKEN", "tok")
	if _, err := loadFeedbackConfig(); err == nil {
		t.Fatal("expected an error with only MAILX_FEEDBACK_INGEST_TOKEN set")
	}
}

func TestFeedbackConfigPartialSetSecretOnlyIsError(t *testing.T) {
	t.Setenv("MAILX_FEEDBACK_CORRELATION_SECRET", "at-least-32-bytes-long-correlation-secret")
	if _, err := loadFeedbackConfig(); err == nil {
		t.Fatal("expected an error with only MAILX_FEEDBACK_CORRELATION_SECRET set")
	}
}
