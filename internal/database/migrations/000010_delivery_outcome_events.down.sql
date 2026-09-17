DROP INDEX IF EXISTS uq_events_message_delivery_attempt;
ALTER TABLE events DROP COLUMN IF EXISTS delivery_attempt_number;
