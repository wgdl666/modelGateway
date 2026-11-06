-- 两张业务表只做逻辑关联；实例退役不删除历史请求，也不创建外键。
CREATE SCHEMA IF NOT EXISTS model_gateway;
CREATE TABLE IF NOT EXISTS model_gateway.model_instances (
 instance_id TEXT PRIMARY KEY,
 model_key TEXT NOT NULL,
 endpoint TEXT NOT NULL,
 machine_id TEXT NOT NULL DEFAULT '',
 gpu_devices INTEGER[] NOT NULL DEFAULT '{}',
 capacity_units INTEGER NOT NULL CHECK (capacity_units > 0),
 used_units INTEGER NOT NULL DEFAULT 0 CHECK (used_units >= 0 AND used_units <= capacity_units),
 ready BOOLEAN NOT NULL DEFAULT FALSE,
 draining BOOLEAN NOT NULL DEFAULT FALSE,
 lease_expires_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS instances_model_idx ON model_gateway.model_instances(model_key);
CREATE TABLE IF NOT EXISTS model_gateway.requests (
 request_id TEXT PRIMARY KEY,
 model_key TEXT NOT NULL,
 request_payload JSONB NOT NULL,
 instance_id TEXT NOT NULL,
 backend_task_id TEXT,
 reserved_units INTEGER NOT NULL CHECK (reserved_units > 0),
 status TEXT NOT NULL CHECK (status IN ('reserved','dispatching','running','succeeded','failed','unknown')),
 result JSONB,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS requests_instance_idx ON model_gateway.requests(instance_id);
CREATE INDEX IF NOT EXISTS requests_pending_idx ON model_gateway.requests(updated_at)
 WHERE status IN ('reserved','dispatching','running','unknown');
