-- Apply before rolling out the admin request media manifest release.
-- Additive and compatible with existing gateway instances. Historical NULL means the
-- list omits the manifest and Ops reads the request detail for those rows.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE model_gateway.requests
    ADD COLUMN IF NOT EXISTS input_media jsonb;
COMMIT;
