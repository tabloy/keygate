-- Where a metered event is billed: the Stripe meter and customer it was
-- recorded against, kept with the row when the usage is recorded. The
-- sync job sends the row there, so editing, clearing or deleting the
-- entitlement, or moving the license to another plan, changes how later
-- usage is billed and not usage already recorded. Rows queued before
-- these columns have them empty and are sent where the license points
-- when they sync, as before.
ALTER TABLE metered_billing
    ADD COLUMN IF NOT EXISTS stripe_meter_event_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS stripe_customer_id      TEXT NOT NULL DEFAULT '';
