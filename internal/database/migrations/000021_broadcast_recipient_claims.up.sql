-- PR review fix (Greptile P1 findings on this branch):
--
-- 1. "Broadcast claims release early": PendingBroadcastRecipients' FOR UPDATE
--    SKIP LOCKED ran as its own implicit (auto-committing) statement, so the
--    row lock was gone before Go-side processing (render/sign/InsertMessage,
--    which charges tenant recipient quota) even began. Two truly-concurrent
--    expander instances could both claim and process the same recipient.
--    Fixed by making the claim ATOMIC: claimed_at is now stamped in the SAME
--    UPDATE statement that performs the SKIP LOCKED select, so the claim and
--    the lock disappear together, and a lease (claimed_at recency) keeps the
--    row unavailable to other claimants for the lease duration even after
--    the lock itself releases — covering the gap across Go-side processing,
--    not just the SQL statement.
--
-- 2. "Materialization failures never terminate": a recipient whose render/
--    build/persist deterministically fails stayed 'pending' forever, was
--    recharged against tenant quota every tick, and could block its
--    broadcast from ever completing. attempts bounds this: after
--    maxRecipientAttempts consecutive failures the recipient moves to the
--    new terminal 'failed' status instead of retrying indefinitely.
ALTER TABLE broadcast_recipients ADD COLUMN claimed_at TIMESTAMPTZ;
ALTER TABLE broadcast_recipients ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

ALTER TABLE broadcast_recipients DROP CONSTRAINT broadcast_recipients_status_check;
ALTER TABLE broadcast_recipients ADD CONSTRAINT broadcast_recipients_status_check
    CHECK (status IN ('pending','suppressed','materialized','failed'));

-- Replaces idx_broadcast_recipients_pending: the claim query now also filters
-- on claimed_at, so the partial index needs it alongside created_at.
DROP INDEX idx_broadcast_recipients_pending;
CREATE INDEX idx_broadcast_recipients_pending ON broadcast_recipients (broadcast_id, created_at, claimed_at) WHERE status = 'pending';
