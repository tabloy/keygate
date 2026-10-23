-- Rate limits, webhook retries, the webhook timeout and the quota
-- warning threshold come from the environment (RATE_LIMIT_*, WEBHOOK_*,
-- QUOTA_WARNING_THRESHOLD). These rows were never read; the dashboard
-- showed them as working settings. The API no longer accepts the keys.
DELETE FROM settings WHERE key IN ('rate_limit_api', 'rate_limit_admin', 'webhook_max_attempts', 'webhook_timeout', 'quota_warning_threshold');
