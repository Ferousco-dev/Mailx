UPDATE api_keys SET scopes = array_remove(array_remove(scopes, 'audiences:read'), 'audiences:write');
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scopes_check CHECK (
    scopes <@ ARRAY[
        'emails:send','emails:read','domains:read','domains:write',
        'webhooks:read','webhooks:write','suppressions:read','suppressions:write',
        'templates:read','templates:write','contacts:read','contacts:write'
    ]::text[]
);

DROP TABLE IF EXISTS audience_members;
ALTER TABLE contacts DROP CONSTRAINT IF EXISTS uq_contacts_tenant_id;
DROP TABLE IF EXISTS audiences;
