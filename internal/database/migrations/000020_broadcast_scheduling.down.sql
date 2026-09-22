DROP INDEX IF EXISTS idx_broadcasts_active_due;
ALTER TABLE broadcasts DROP COLUMN send_at;
