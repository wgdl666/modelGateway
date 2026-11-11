-- 切换前停止旧版受理与 Worker，再执行回填；不删除请求或释放已有占用。
-- 模型名称只在这次历史迁移中映射，运行时必须读取登记的 protocol。
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE model_gateway.model_instances ADD COLUMN IF NOT EXISTS protocol text;
ALTER TABLE model_gateway.requests ADD COLUMN IF NOT EXISTS protocol text;
WITH known(model_key, protocol) AS (VALUES
    ('Vmind_wardrobe_v1.0', 'garment_extraction'),
    ('Vmind_cloth_gen_v1.0', 'product_generation'),
    ('Vmind_outfit_v1.0', 'vton'),
    ('Vmind_bodymodel_v1.0', 'body_model'),
    ('Vmind_outfit_video_v1.0', 'outfit_video'),
    ('Vmind_human_parse_v1.0', 'human_parse'),
    ('Vmind_human_yolo_v1.0', 'human_yolo'),
    ('human-yolo', 'human_yolo'),
    ('Vmind_segment_person_bria_v1.0', 'segment_person'),
    ('Vmind_segment_subject_bria_v1.0', 'segment_subject'),
    ('Vmind_lens_distortion_correction_v1.0', 'lens_correction'),
    ('lens-distortion-correction', 'lens_correction'),
    ('Vmind_dream_v1', 'chat_completions'),
    ('Vmind_decision_v1', 'decision'),
    ('facesim', 'face_compare')
)
UPDATE model_gateway.model_instances AS row SET protocol = known.protocol FROM known
WHERE row.model_key = known.model_key AND row.protocol IS NULL;
WITH known(model_key, protocol) AS (VALUES
    ('Vmind_wardrobe_v1.0', 'garment_extraction'),
    ('Vmind_cloth_gen_v1.0', 'product_generation'),
    ('Vmind_outfit_v1.0', 'vton'),
    ('Vmind_bodymodel_v1.0', 'body_model'),
    ('Vmind_outfit_video_v1.0', 'outfit_video'),
    ('Vmind_human_parse_v1.0', 'human_parse'),
    ('Vmind_human_yolo_v1.0', 'human_yolo'),
    ('human-yolo', 'human_yolo'),
    ('Vmind_segment_person_bria_v1.0', 'segment_person'),
    ('Vmind_segment_subject_bria_v1.0', 'segment_subject'),
    ('Vmind_lens_distortion_correction_v1.0', 'lens_correction'),
    ('lens-distortion-correction', 'lens_correction'),
    ('Vmind_dream_v1', 'chat_completions'),
    ('Vmind_decision_v1', 'decision'),
    ('facesim', 'face_compare')
)
UPDATE model_gateway.requests AS row SET protocol = known.protocol FROM known
WHERE row.model_key = known.model_key AND row.protocol IS NULL;
-- 未知历史登记必须先核对协议，禁止把不明记录当成默认图片任务继续执行。
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM model_gateway.model_instances WHERE protocol IS NULL)
        OR EXISTS (SELECT 1 FROM model_gateway.requests WHERE protocol IS NULL) THEN
        RAISE EXCEPTION 'unmapped model_key: assign its protocol before applying this migration';
    END IF;
END $$;
ALTER TABLE model_gateway.model_instances ALTER COLUMN protocol SET NOT NULL;
ALTER TABLE model_gateway.requests ALTER COLUMN protocol SET NOT NULL;
COMMIT;
