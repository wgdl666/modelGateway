-- 在发布统一调度网关前显式执行。仅增加派发快照与规范模型键，不改变历史请求身份。
BEGIN;
SET LOCAL search_path = model_gateway, public;
SET LOCAL lock_timeout = '5s';
LOCK TABLE model_instances IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE requests ADD COLUMN IF NOT EXISTS resource_groups jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE requests ALTER COLUMN queue_publish_at DROP DEFAULT;

-- 旧网关只有单卡占用；将其实际派发归属补入新快照，滚动期间继续兼容旧单卡记录。
UPDATE requests SET resource_groups = jsonb_build_array(resource_group)
WHERE slot_reserved AND resource_groups = '[]'::jsonb AND resource_group <> '';

-- 两个旧键原来只做同步转发；如已有占用，先排空，不能靠改名绕过在途容量。
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM requests
    WHERE model_key IN ('human-yolo', 'lens-distortion-correction')
      AND (state IN ('queued', 'processing') OR slot_reserved)
  ) THEN
    RAISE EXCEPTION 'drain old human-yolo/lens-distortion-correction requests before model-key migration';
  END IF;
END $$;

UPDATE model_instances
SET model_key = CASE model_key
    WHEN 'human-yolo' THEN 'Vmind_human_yolo_v1.0'
    WHEN 'lens-distortion-correction' THEN 'Vmind_lens_distortion_correction_v1.0'
  END,
  updated_at = now()
WHERE model_key IN ('human-yolo', 'lens-distortion-correction');

COMMIT;
