ALTER TABLE events DROP CONSTRAINT events_event_type_check;
ALTER TABLE events ADD CONSTRAINT events_event_type_check
    CHECK (event_type IN ('queued','delivery_attempted','delivered','deferred','bounced','failed','suppressed','complained','opened','clicked'));
