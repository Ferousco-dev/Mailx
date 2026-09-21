-- Downgrade: suppression state is discarded and rows using the new vocabulary are
-- mapped to the closest pre-v0.30 value so the old constraints can be restored.
DELETE FROM events WHERE event_type = 'suppressed';
DROP INDEX IF EXISTS uq_events_message_suppressed;
ALTER TABLE events DROP CONSTRAINT events_event_type_check;
ALTER TABLE events ADD CONSTRAINT events_event_type_check
    CHECK (event_type IN ('queued','delivery_attempted','delivered','deferred','bounced','failed'));

UPDATE recipients SET status = 'failed' WHERE status = 'suppressed';
ALTER TABLE recipients DROP CONSTRAINT recipients_status_check;
ALTER TABLE recipients ADD CONSTRAINT recipients_status_check CHECK (status IN ('pending','delivered','failed'));

UPDATE messages SET status = 'failed' WHERE status = 'suppressed';
ALTER TABLE messages DROP CONSTRAINT messages_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_status_check
    CHECK (status IN ('queued','processing','retrying','delivered','failed','bounced'));

UPDATE webhook_subscriptions SET event_types = array_remove(event_types, 'email.suppressed')
    WHERE 'email.suppressed' = ANY(event_types);
DELETE FROM webhook_subscriptions WHERE cardinality(event_types) = 0;
ALTER TABLE webhook_subscriptions DROP CONSTRAINT webhook_subscriptions_event_types_check;
ALTER TABLE webhook_subscriptions ADD CONSTRAINT webhook_subscriptions_event_types_check CHECK (
    cardinality(event_types) > 0 AND
    event_types <@ ARRAY['email.queued','email.delivered','email.delivery_delayed','email.failed','email.bounced']::text[]
);

UPDATE api_keys SET scopes = array_remove(array_remove(scopes, 'suppressions:read'), 'suppressions:write');
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY['emails:send','emails:read','domains:read','domains:write','webhooks:read','webhooks:write']::text[]
);

DROP TABLE IF EXISTS suppressions;
