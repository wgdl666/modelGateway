-- 停止旧网关写入和消费后执行。整笔事务提交前，新网关会拒绝连接旧表。
BEGIN;
SET LOCAL lock_timeout = '5s';
LOCK TABLE model_gateway.model_instances, model_gateway.requests IN ACCESS EXCLUSIVE MODE;

DO $migration$
DECLARE
    old_instances boolean;
    old_requests boolean;
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_catalog.pg_attribute
        WHERE attrelid = 'model_gateway.model_instances'::regclass
          AND attname = 'capability' AND NOT attisdropped
    ) INTO old_instances;
    SELECT EXISTS (
        SELECT 1 FROM pg_catalog.pg_attribute
        WHERE attrelid = 'model_gateway.requests'::regclass
          AND attname = 'capability' AND NOT attisdropped
    ) INTO old_requests;
    IF old_instances <> old_requests THEN
        RAISE EXCEPTION 'model key migration requires both legacy columns or neither';
    END IF;
    IF NOT old_instances THEN
        RETURN;
    END IF;

    -- 旧值仅用于识别历史业务，运行时代码以 model_key 为唯一入口。
    CREATE TEMP TABLE gateway_model_keys (model_key text PRIMARY KEY, capability text NOT NULL) ON COMMIT DROP;
    INSERT INTO pg_temp.gateway_model_keys VALUES
        ('Vmind_wardrobe_v1.0', 'garment_extraction'),
        ('Vmind_cloth_gen_v1.0', 'cloth_gen'),
        ('Vmind_outfit_v1.0', 'vton'),
        ('Vmind_bodymodel_v1.0', 'body_model'),
        ('Vmind_outfit_video_v1.0', 'video');

    -- 已对外失败仍可能占槽。历史业务占用未核销时，整次切换应回滚。
    IF EXISTS (
        SELECT 1 FROM model_gateway.requests AS r
        LEFT JOIN pg_temp.gateway_model_keys AS rk
            ON (r.model_key, r.capability) = (rk.model_key, rk.capability)
        LEFT JOIN model_gateway.model_instances AS i ON i.instance_id = r.instance_id
        LEFT JOIN pg_temp.gateway_model_keys AS ik
            ON (i.model_key, i.capability) = (ik.model_key, ik.capability)
        WHERE r.slot_reserved AND (rk.model_key IS NULL OR (i.instance_id IS NOT NULL AND ik.model_key IS NULL))
    ) THEN
        RAISE EXCEPTION 'model key migration blocked by a reserved legacy request; resolve its original execution first';
    END IF;

    -- 同一旧键曾接商品图和拆衣。归档旧登记，保留 ID 给已完成请求追溯。
    UPDATE model_gateway.model_instances AS i
    SET deleted_at = CURRENT_TIMESTAMP, draining = true, updated_at = CURRENT_TIMESTAMP
    WHERE i.deleted_at IS NULL AND NOT EXISTS (
        SELECT 1 FROM pg_temp.gateway_model_keys AS k
        WHERE (i.model_key, i.capability) = (k.model_key, k.capability)
    );

    -- 仅终结尚未派发的旧业务请求。改写原模型键会破坏请求幂等与审计。
    UPDATE model_gateway.requests AS r
    SET state = 'failed',
        error_code = CASE WHEN r.deadline_at <= CURRENT_TIMESTAMP THEN 'queue_timeout' ELSE 'model_retired' END,
        error_message = CASE WHEN r.deadline_at <= CURRENT_TIMESTAMP THEN 'request expired before dispatch'
                             ELSE 'submit a new request with the current model key' END,
        error_status = CASE WHEN r.deadline_at <= CURRENT_TIMESTAMP THEN 504 ELSE 409 END,
        queue_publish_at = NULL, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
    WHERE r.state = 'queued' AND NOT EXISTS (
        SELECT 1 FROM pg_temp.gateway_model_keys AS k
        WHERE (r.model_key, r.capability) = (k.model_key, k.capability)
    );

    -- 历史能力只用于审计。新写入省略这列，旧账本的模型和执行归属继续可查。
    ALTER TABLE model_gateway.model_instances RENAME COLUMN capability TO legacy_capability;
    ALTER TABLE model_gateway.requests RENAME COLUMN capability TO legacy_capability;
    ALTER TABLE model_gateway.model_instances ALTER COLUMN legacy_capability DROP NOT NULL,
        ALTER COLUMN legacy_capability DROP DEFAULT;
    ALTER TABLE model_gateway.requests ALTER COLUMN legacy_capability DROP NOT NULL,
        ALTER COLUMN legacy_capability DROP DEFAULT;
END;
$migration$;

DROP INDEX IF EXISTS model_gateway.modelinstance_model_key_capability;
DROP INDEX IF EXISTS model_gateway.request_model_key_capability_created_at_request_id;
CREATE INDEX IF NOT EXISTS modelinstance_model_key ON model_gateway.model_instances (model_key);
CREATE INDEX IF NOT EXISTS request_model_key_created_at_request_id
    ON model_gateway.requests (model_key, created_at, request_id) WHERE state = 'queued';
COMMIT;
