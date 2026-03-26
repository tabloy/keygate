DROP INDEX IF EXISTS idx_licenses_key_hash;
ALTER TABLE licenses DROP COLUMN IF EXISTS key_hash;
