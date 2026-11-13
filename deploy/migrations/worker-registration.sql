-- Apply after deployment-lifecycle.sql, before enabling self-registering workers.
-- The earlier operation/agent tables remain unused; do not drop historical data.
BEGIN;
ALTER TABLE model_gateway.model_instances ADD COLUMN IF NOT EXISTS worker_id text NOT NULL DEFAULT '';
ALTER TABLE model_gateway.model_instances ADD COLUMN IF NOT EXISTS boot_id text NOT NULL DEFAULT '';
ALTER TABLE model_gateway.model_instances ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;
ALTER TABLE model_gateway.model_instances ADD COLUMN IF NOT EXISTS lifecycle_draining boolean NOT NULL DEFAULT false;
ALTER TABLE model_gateway.requests ADD COLUMN IF NOT EXISTS boot_id text NOT NULL DEFAULT '';
COMMIT;
