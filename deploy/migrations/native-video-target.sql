-- 先加列，再用新登记绑定 LTX 目标和 GPU 分组，保留旧实例的任务归属。
ALTER TABLE model_gateway.model_instances
    ADD COLUMN IF NOT EXISTS target_instance text NOT NULL DEFAULT '';
