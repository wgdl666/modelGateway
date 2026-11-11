-- Apply before rolling out the generation receipt gateway release.
-- Additive and compatible with existing gateway instances. Historical 0 means unrecorded.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE model_gateway.requests
    ADD COLUMN IF NOT EXISTS dispatch_attempts bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dispatch_retry_reason text NOT NULL DEFAULT '';
COMMIT;
