-- Reverse: metered billing
DROP INDEX IF EXISTS idx_metered_unsynced;
DROP INDEX IF EXISTS idx_metered_license;
DROP TABLE IF EXISTS metered_billing;

-- Reverse: license addons
DROP INDEX IF EXISTS idx_license_addons_license;
DROP TABLE IF EXISTS license_addons;

-- Reverse: addons
DROP INDEX IF EXISTS idx_addons_product;
DROP TABLE IF EXISTS addons;

-- Reverse: plans floating columns
ALTER TABLE plans DROP COLUMN IF EXISTS floating_timeout;
ALTER TABLE plans DROP COLUMN IF EXISTS license_model;

-- Reverse: floating sessions
DROP INDEX IF EXISTS idx_floating_expires;
DROP INDEX IF EXISTS idx_floating_license;
DROP TABLE IF EXISTS floating_sessions;
