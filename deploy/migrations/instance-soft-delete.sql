-- 网关更新前增加可空字段，已有实例继续显示，历史请求仍保留原实例 ID。
ALTER TABLE model_gateway.model_instances
    ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
