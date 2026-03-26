-- Keygate: License Management Platform
-- Migration: initial schema

-- ─── Users & OAuth ───

CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE oauth_accounts (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    email       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(provider, provider_id)
);
CREATE INDEX idx_oauth_user ON oauth_accounts(user_id);

-- ─── Products & API Keys ───

CREATE TABLE products (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    type       TEXT NOT NULL CHECK (type IN ('desktop', 'saas', 'hybrid')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id         TEXT PRIMARY KEY,
    product_id TEXT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    prefix     TEXT NOT NULL,
    scopes     TEXT[] NOT NULL DEFAULT '{}',
    last_used  TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_apikeys_product ON api_keys(product_id);

-- ─── Plans & Entitlements ───

CREATE TABLE plans (
    id               TEXT PRIMARY KEY,
    product_id       TEXT NOT NULL REFERENCES products(id),
    name             TEXT NOT NULL,
    slug             TEXT NOT NULL,
    license_type     TEXT NOT NULL CHECK (license_type IN ('subscription', 'perpetual', 'trial')),
    billing_interval TEXT NOT NULL DEFAULT '' CHECK (billing_interval IN ('', 'month', 'year')),
    max_activations  INTEGER NOT NULL DEFAULT 3,
    trial_days       INTEGER NOT NULL DEFAULT 0,
    grace_days       INTEGER NOT NULL DEFAULT 7,
    stripe_price_id  TEXT NOT NULL DEFAULT '',
    paypal_plan_id   TEXT NOT NULL DEFAULT '',
    active           BOOLEAN NOT NULL DEFAULT true,
    sort_order       INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(product_id, slug)
);

CREATE TABLE entitlements (
    id         TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    feature    TEXT NOT NULL,
    value_type TEXT NOT NULL CHECK (value_type IN ('bool', 'int', 'string')),
    value      TEXT NOT NULL DEFAULT '',
    UNIQUE(plan_id, feature)
);

-- ─── Licenses ───

CREATE TABLE licenses (
    id                       TEXT PRIMARY KEY,
    product_id               TEXT NOT NULL REFERENCES products(id),
    plan_id                  TEXT NOT NULL REFERENCES plans(id),
    user_id                  TEXT REFERENCES users(id) ON DELETE SET NULL,
    email                    TEXT NOT NULL,
    license_key              TEXT NOT NULL UNIQUE,
    payment_provider         TEXT NOT NULL DEFAULT '',
    stripe_customer_id       TEXT NOT NULL DEFAULT '',
    stripe_subscription_id   TEXT UNIQUE,
    paypal_subscription_id   TEXT UNIQUE,
    status                   TEXT NOT NULL DEFAULT 'active'
                             CHECK (status IN ('active','trialing','past_due','canceled','expired','suspended','revoked')),
    valid_from               TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until              TIMESTAMPTZ,
    canceled_at              TIMESTAMPTZ,
    suspended_at             TIMESTAMPTZ,
    notes                    TEXT NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_licenses_email ON licenses(email);
CREATE INDEX idx_licenses_key ON licenses(license_key);
CREATE INDEX idx_licenses_product_status ON licenses(product_id, status);
CREATE INDEX idx_licenses_user ON licenses(user_id);
CREATE INDEX idx_licenses_stripe_sub ON licenses(stripe_subscription_id);
CREATE INDEX idx_licenses_paypal_sub ON licenses(paypal_subscription_id);

-- ─── Activations ───

CREATE TABLE activations (
    id              TEXT PRIMARY KEY,
    license_id      TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    identifier      TEXT NOT NULL,
    identifier_type TEXT NOT NULL CHECK (identifier_type IN ('device', 'user')),
    label           TEXT NOT NULL DEFAULT '',
    ip_address      TEXT NOT NULL DEFAULT '',
    last_verified   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(license_id, identifier)
);
CREATE INDEX idx_activations_license ON activations(license_id);

-- ─── Audit Log ───

CREATE TABLE audit_logs (
    id         TEXT PRIMARY KEY,
    entity     TEXT NOT NULL,
    entity_id  TEXT NOT NULL,
    action     TEXT NOT NULL,
    actor_id   TEXT NOT NULL DEFAULT '',
    actor_type TEXT NOT NULL DEFAULT '',
    changes    JSONB NOT NULL DEFAULT '{}',
    ip_address TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_entity ON audit_logs(entity, entity_id);
CREATE INDEX idx_audit_created ON audit_logs(created_at);
