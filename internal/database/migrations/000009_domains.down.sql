DROP TABLE IF EXISTS domains;
-- Keep the expanded API-key scope CHECK on rollback. Removing domain scopes
-- from historical credentials would require destructive credential mutation;
-- older application code safely ignores those scopes because authorization is
-- route-specific. Reapplying this migration replaces the constraint cleanly.
