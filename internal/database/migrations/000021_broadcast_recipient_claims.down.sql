DROP INDEX idx_broadcast_recipients_pending;
CREATE INDEX idx_broadcast_recipients_pending ON broadcast_recipients (broadcast_id, created_at) WHERE status = 'pending';

UPDATE broadcast_recipients SET status = 'pending' WHERE status = 'failed';

ALTER TABLE broadcast_recipients DROP CONSTRAINT broadcast_recipients_status_check;
ALTER TABLE broadcast_recipients ADD CONSTRAINT broadcast_recipients_status_check
    CHECK (status IN ('pending','suppressed','materialized'));

ALTER TABLE broadcast_recipients DROP COLUMN attempts;
ALTER TABLE broadcast_recipients DROP COLUMN claimed_at;
