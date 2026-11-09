-- 数据库发布先执行本文件。默认值把已有请求和滚动发布期间旧副本接收的请求纳入投递。
ALTER TABLE model_gateway.requests
    ADD COLUMN IF NOT EXISTS queue_publish_at timestamptz DEFAULT CURRENT_TIMESTAMP;

CREATE INDEX CONCURRENTLY IF NOT EXISTS request_queue_publish_at_request_id
    ON model_gateway.requests (queue_publish_at, request_id)
    WHERE state = 'queued' AND queue_publish_at IS NOT NULL;
