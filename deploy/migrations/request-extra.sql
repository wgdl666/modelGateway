-- Apply before rolling out the admin request media manifest release.
-- Additive aside from dropping input_media, which was only added by an unreleased draft
-- and is not read by any running gateway. NULL extra means the list omits the manifest.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE model_gateway.requests
    ADD COLUMN IF NOT EXISTS extra jsonb;
ALTER TABLE model_gateway.requests
    DROP COLUMN IF EXISTS input_media;
COMMIT;
