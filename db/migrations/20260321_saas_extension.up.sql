-- =====================================================
-- Migration: SaaS licensing extension + analytics
-- Additive only — no DROP on existing data columns
-- =====================================================

-- ─── Enhanced Entitlements ───
ALTER TABLE entitlements DROP CONSTRAINT IF EXISTS entitlements_value_type_check;
ALTER TABLE entitlements ADD CONSTRAINT entitlements_value_type_check
    CHECK (value_type IN ('bool', 'int', 'string', 'quota', 'flag'));

ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS quota_period TEXT NOT NULL DEFAULT ''
    CHECK (quota_period IN ('', 'hourly', 'daily', 'monthly', 'yearly'));
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS quota_unit TEXT NOT NULL DEFAULT '';

-- ─── Plans: seat support ───
ALTER TABLE plans ADD COLUMN IF NOT EXISTS max_seats INTEGER NOT NULL DEFAULT 0;

-- ─── Licenses: org binding ───
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS org_name TEXT NOT NULL DEFAULT '';

-- ─── Seats (team members per license) ───
CREATE TABLE IF NOT EXISTS seats (
    id          TEXT PRIMARY KEY,
    license_id  TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    user_id     TEXT REFERENCES users(id) ON DELETE SET NULL,
    email       TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    invited_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    accepted_at TIMESTAMPTZ,
    removed_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(license_id, email)
);
CREATE INDEX IF NOT EXISTS idx_seats_license ON seats(license_id);
CREATE INDEX IF NOT EXISTS idx_seats_email ON seats(email);
CREATE INDEX IF NOT EXISTS idx_seats_user ON seats(user_id);

-- ─── Usage Events (metering) ───
CREATE TABLE IF NOT EXISTS usage_events (
    id          TEXT PRIMARY KEY,
    license_id  TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    feature     TEXT NOT NULL,
    quantity    BIGINT NOT NULL DEFAULT 1,
    metadata    JSONB NOT NULL DEFAULT '{}',
    ip_address  TEXT NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_usage_license_feature ON usage_events(license_id, feature, recorded_at);
CREATE INDEX IF NOT EXISTS idx_usage_recorded ON usage_events(recorded_at);

-- ─── Usage Counters (pre-aggregated for fast quota checks) ───
CREATE TABLE IF NOT EXISTS usage_counters (
    id          TEXT PRIMARY KEY,
    license_id  TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    feature     TEXT NOT NULL,
    period      TEXT NOT NULL,
    period_key  TEXT NOT NULL,
    used        BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(license_id, feature, period, period_key)
);
CREATE INDEX IF NOT EXISTS idx_counters_lookup ON usage_counters(license_id, feature, period, period_key);

-- ─── Webhooks (outbound, per-product) ───
CREATE TABLE IF NOT EXISTS webhooks (
    id          TEXT PRIMARY KEY,
    product_id  TEXT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    events      TEXT[] NOT NULL DEFAULT '{}',
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhooks_product ON webhooks(product_id);

-- ─── Webhook Deliveries ───
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id            TEXT PRIMARY KEY,
    webhook_id    TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event         TEXT NOT NULL,
    payload       JSONB NOT NULL DEFAULT '{}',
    response_code INTEGER,
    response_body TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    next_retry    TIMESTAMPTZ,
    status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivered','failed')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_deliveries_webhook ON webhook_deliveries(webhook_id);
CREATE INDEX IF NOT EXISTS idx_deliveries_retry ON webhook_deliveries(status, next_retry)
    WHERE status = 'pending';

-- ─── Analytics: daily snapshots ───
CREATE TABLE IF NOT EXISTS analytics_snapshots (
    id              TEXT PRIMARY KEY,
    date            DATE NOT NULL,
    product_id      TEXT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    total_licenses  INTEGER NOT NULL DEFAULT 0,
    active_licenses INTEGER NOT NULL DEFAULT 0,
    new_licenses    INTEGER NOT NULL DEFAULT 0,
    churned         INTEGER NOT NULL DEFAULT 0,
    total_activations INTEGER NOT NULL DEFAULT 0,
    total_seats     INTEGER NOT NULL DEFAULT 0,
    total_usage     BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(date, product_id)
);
CREATE INDEX IF NOT EXISTS idx_analytics_product_date ON analytics_snapshots(product_id, date);

-- ─── Subscriptions (user-facing subscription management) ───
CREATE TABLE IF NOT EXISTS subscriptions (
    id                  TEXT PRIMARY KEY,
    license_id          TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    user_id             TEXT REFERENCES users(id) ON DELETE SET NULL,
    plan_id             TEXT NOT NULL REFERENCES plans(id),
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active','past_due','canceled','expired','paused')),
    payment_provider    TEXT NOT NULL DEFAULT '',
    external_id         TEXT NOT NULL DEFAULT '',
    current_period_start TIMESTAMPTZ,
    current_period_end   TIMESTAMPTZ,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT false,
    canceled_at         TIMESTAMPTZ,
    trial_start         TIMESTAMPTZ,
    trial_end           TIMESTAMPTZ,
    metadata            JSONB NOT NULL DEFAULT '{}',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_subscriptions_license ON subscriptions(license_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_user ON subscriptions(user_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_external ON subscriptions(payment_provider, external_id);
