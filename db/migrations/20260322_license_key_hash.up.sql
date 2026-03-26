-- Add hashed license key column for secure lookups
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS key_hash TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_licenses_key_hash ON licenses(key_hash) WHERE key_hash != '';

-- Backfill: hash existing keys (done by application on startup)
