-- Time-critical send priority: "normal" (default, unchanged behavior) or
-- "urgent" (OTPs/password resets — see internal/retry.UrgentBackoffPolicy).
-- The worker's actual retry-schedule decision reads Priority straight from
-- storage.StoredMessageMetadata (no DB round trip on the hot per-job path);
-- this column exists for API visibility/filtering and durable record-keeping,
-- not as the worker's source of truth.
ALTER TABLE messages ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('normal','urgent'));
