-- Fix subscription status check constraint to include all license statuses.
-- Subscriptions must sync with license status which can be any of these values.
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('active','trialing','past_due','canceled','expired','paused','suspended','revoked'));
