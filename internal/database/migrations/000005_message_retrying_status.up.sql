-- v0.18: worker.Pool is now wired to report each job's retry outcome
-- (see internal/worker.WithStatusReporter), so "temporary failure,
-- another attempt is scheduled" becomes a real, truthfully distinct
-- status instead of staying indistinguishable from 'queued'/'processing'.
ALTER TABLE messages DROP CONSTRAINT messages_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_status_check
    CHECK (status IN ('queued','processing','retrying','delivered','failed','bounced'));
