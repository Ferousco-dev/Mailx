ALTER TABLE delivery_attempts DROP CONSTRAINT delivery_attempts_transport_kind_check;
ALTER TABLE delivery_attempts DROP COLUMN effective_source_ip;
ALTER TABLE delivery_attempts DROP COLUMN effective_hostname;
ALTER TABLE delivery_attempts DROP COLUMN transport_kind;
ALTER TABLE delivery_attempts DROP COLUMN sending_member_id;

ALTER TABLE messages DROP COLUMN sending_member_id;

ALTER TABLE domains DROP COLUMN sending_pool_id;

DROP INDEX idx_sending_pool_members_pool;

DROP TABLE sending_pool_members;

DROP TABLE sending_pools;
