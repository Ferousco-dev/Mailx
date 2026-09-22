-- v0.37 scheduled sending for Broadcasts. Normal email scheduling already
-- existed since v0.18 (messages/outbox.available_at); this extends the SAME
-- "not-before eligibility" concept to the existing v0.36 expansion poller
-- instead of building a second scheduler. send_at is NULL for an immediate
-- broadcast (unchanged behavior) or a future instant below which expansion
-- must never start.
--
-- Deliberately NOT touched: audience_snapshot_at. It stays stamped at
-- acceptance time regardless of send_at, so the RSK-038 snapshot boundary is
-- unaffected by scheduling — a member added to the Audience after acceptance
-- is excluded whether the broadcast sends immediately or a week later.
ALTER TABLE broadcasts ADD COLUMN send_at TIMESTAMPTZ;

-- Query: "which active broadcasts are due" (ClaimActiveBroadcasts). Most
-- active broadcasts have send_at NULL (immediate); this index lets the
-- planner find NULL and due rows without scanning completed/failed
-- broadcasts, matching idx_broadcasts_active's existing partial-index
-- pattern.
CREATE INDEX idx_broadcasts_active_due ON broadcasts (send_at, created_at) WHERE status IN ('accepted','expanding');
