-- The first segment of the license keys a product issues (issue #30),
-- so a key shows which product it belongs to. Existing products keep
-- KG, the prefix every key had until now. Keys are found by a hash of
-- the whole key, so a later change applies to new keys only and the
-- ones already issued keep working.
ALTER TABLE products ADD COLUMN IF NOT EXISTS license_prefix VARCHAR(8) NOT NULL DEFAULT 'KG';
ALTER TABLE products DROP CONSTRAINT IF EXISTS products_license_prefix_check;
ALTER TABLE products ADD CONSTRAINT products_license_prefix_check
    CHECK (license_prefix ~ '^[A-Z0-9]{2,8}$');
