-- Apply after every eligible Wardrobe worker supports both reference modes.
UPDATE model_gateway.model_instances
SET input_schema = jsonb_set(input_schema, '{properties,bbox_only}',
    '{"type":"boolean","default":true,"description":"默认仅使用裁切图。false 时依次使用原图、裁切图。"}'::jsonb)
WHERE model_key = 'Vmind_wardrobe_v1.0'
  AND protocol = 'garment_extraction'
  AND jsonb_typeof(input_schema->'properties') = 'object'
  AND NOT (input_schema->'properties' ? 'bbox_only');
