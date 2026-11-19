-- Apply only where all eligible Qwen VTON and BodyModel workers support RGBA PNG.
UPDATE model_gateway.model_instances
SET input_schema = jsonb_set(input_schema, '{properties,transparent_background}',
    '{"type":"boolean","default":false,"description":"返回带 Alpha 通道的透明背景 PNG"}'::jsonb)
WHERE (model_key, protocol) IN (
    ('Vmind_outfit_v1.0', 'vton'),
    ('Vmind_bodymodel_v1.0', 'body_model')
)
  AND jsonb_typeof(input_schema->'properties') = 'object'
  AND NOT (input_schema->'properties' ? 'transparent_background');
