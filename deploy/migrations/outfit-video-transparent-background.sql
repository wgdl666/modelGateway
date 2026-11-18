-- Enable the optional field on existing compatible outfit-video instances.
UPDATE model_gateway.model_instances
SET input_schema = jsonb_set(input_schema, '{properties,transparent_background}',
    '{"type":"boolean","default":false,"description":"返回带透明通道的 WebM 视频"}'::jsonb)
WHERE model_key = 'Vmind_outfit_video_v1.0'
  AND protocol = 'outfit_video'
  AND jsonb_typeof(input_schema->'properties') = 'object'
  AND NOT (input_schema->'properties' ? 'transparent_background');
