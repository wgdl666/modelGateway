-- 仅美东 ppe_exhibition：FaceSim 是 OpenCV CPU 推理，建立独立并发组，不冒充任何 GPU。
-- 执行后通过管理登记接口提交 deploy/exhibition-facesim-instance.json。
BEGIN;
INSERT INTO model_gateway.resource_groups (resource_group, machine_id, gpu_device, capacity_units)
VALUES ('g6/cpu/facesim', '', NULL, 1)
ON CONFLICT (resource_group) DO NOTHING;
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM model_gateway.resource_groups
    WHERE resource_group = 'g6/cpu/facesim' AND (machine_id <> '' OR gpu_device IS NOT NULL)
  ) THEN
    RAISE EXCEPTION 'g6/cpu/facesim must not have a physical GPU binding';
  END IF;
END $$;
COMMIT;
