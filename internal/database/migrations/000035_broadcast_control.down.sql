UPDATE broadcasts SET status = 'failed', failure_reason = 'cancelled'
    WHERE status = 'cancelled';
ALTER TABLE broadcasts DROP CONSTRAINT broadcasts_status_check;
ALTER TABLE broadcasts ADD CONSTRAINT broadcasts_status_check
    CHECK (status IN ('accepted','expanding','completed','failed'));
