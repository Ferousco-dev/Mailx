ALTER TABLE broadcasts DROP CONSTRAINT broadcasts_status_check;
ALTER TABLE broadcasts ADD CONSTRAINT broadcasts_status_check
    CHECK (status IN ('accepted','expanding','completed','failed','cancelled'));
