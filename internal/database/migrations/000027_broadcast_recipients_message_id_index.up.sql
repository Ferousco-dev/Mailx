-- PurgeExpiredMessages (v0.46, internal/database/retention.go) runs
-- `UPDATE broadcast_recipients SET message_id = NULL WHERE message_id = ANY($1)`
-- on every purge cycle, but broadcast_recipients.message_id had no
-- supporting index at all (unlike every other message_id column in this
-- schema) - a large expired backlog would force a sequential scan
-- competing with live traffic. Partial (WHERE message_id IS NOT NULL)
-- matches this codebase's existing partial-index convention (see migration
-- 000010) since most broadcast_recipients rows never had a message_id set.
CREATE INDEX idx_broadcast_recipients_message_id ON broadcast_recipients (message_id)
    WHERE message_id IS NOT NULL;
