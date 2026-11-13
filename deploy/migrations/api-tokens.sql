-- 在发布支持独立 Token 的网关前执行；不修改现有固定凭据或请求身份。
BEGIN;
CREATE TABLE IF NOT EXISTS model_gateway.api_tokens (
 id varchar PRIMARY KEY,
 name varchar NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
 encrypted_token text NOT NULL,
 created_by varchar NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz NULL
);
-- 阿里云 RDS 已有同名触发器函数但不支持安装扩展，优先复用。
DO $$
BEGIN
 IF to_regprocedure('public.moddatetime()') IS NULL THEN
  CREATE EXTENSION IF NOT EXISTS moddatetime WITH SCHEMA public;
 END IF;
END $$;
DROP TRIGGER IF EXISTS api_tokens_updated_at ON model_gateway.api_tokens;
CREATE TRIGGER api_tokens_updated_at BEFORE UPDATE ON model_gateway.api_tokens
 FOR EACH ROW EXECUTE PROCEDURE public.moddatetime(updated_at);
ALTER TABLE model_gateway.requests ADD COLUMN IF NOT EXISTS api_token_id varchar NOT NULL DEFAULT '';
COMMIT;
