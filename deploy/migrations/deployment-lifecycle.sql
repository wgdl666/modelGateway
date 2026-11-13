-- Apply before deploying lifecycle-aware gateways. No historical identities are rewritten.
BEGIN;
ALTER TABLE model_gateway.model_instances ADD COLUMN IF NOT EXISTS deployment_id text NOT NULL DEFAULT '';
ALTER TABLE model_gateway.requests ADD COLUMN IF NOT EXISTS deployment_id text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS model_gateway.deployment_operations (
 operation_id text PRIMARY KEY, region text NOT NULL, worker_id text NOT NULL,
 manifest jsonb NOT NULL, manifest_sha256 text NOT NULL,
 phase text NOT NULL DEFAULT 'planned', snapshot jsonb NOT NULL DEFAULT '{}',
 receipts jsonb NOT NULL DEFAULT '{}', error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS model_gateway.deployment_leases (
 resource_group text PRIMARY KEY, region text NOT NULL, worker_id text NOT NULL,
 operation_id text NOT NULL DEFAULT '', owner text NOT NULL DEFAULT '',
 epoch bigint NOT NULL DEFAULT 0, lease_until timestamptz NOT NULL DEFAULT 'epoch'
);
-- Bind this singleton explicitly to the regional environment before enabling deployments.
-- Example: INSERT INTO model_gateway.deployment_domain(id,region) VALUES(1,'cn-shenzhen/dev');
CREATE TABLE IF NOT EXISTS model_gateway.deployment_domain (
 id bigint PRIMARY KEY CHECK(id=1), region text NOT NULL UNIQUE
);
COMMIT;
