DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS analytics_snapshots;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhooks;
DROP TABLE IF EXISTS usage_counters;
DROP TABLE IF EXISTS usage_events;
DROP TABLE IF EXISTS seats;

ALTER TABLE licenses DROP COLUMN IF EXISTS org_name;
ALTER TABLE plans DROP COLUMN IF EXISTS max_seats;
ALTER TABLE entitlements DROP COLUMN IF EXISTS quota_unit;
ALTER TABLE entitlements DROP COLUMN IF EXISTS quota_period;

ALTER TABLE entitlements DROP CONSTRAINT IF EXISTS entitlements_value_type_check;
ALTER TABLE entitlements ADD CONSTRAINT entitlements_value_type_check
    CHECK (value_type IN ('bool', 'int', 'string'));
