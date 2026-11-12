-- 发布媒体清单功能前新增可空的 extra；已有请求保持 NULL，不回填历史数据。
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE model_gateway.requests
    ADD COLUMN IF NOT EXISTS extra jsonb;
COMMIT;
