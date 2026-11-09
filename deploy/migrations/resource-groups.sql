-- 在暂停实例登记期间运行。已有请求保留组标识和占槽记录。
BEGIN;
LOCK TABLE model_gateway.model_instances IN SHARE MODE;

CREATE TABLE IF NOT EXISTS model_gateway.resource_groups (
    resource_group text PRIMARY KEY,
    machine_id text NOT NULL DEFAULT '',
    gpu_device bigint,
    capacity_units bigint NOT NULL,
    CONSTRAINT resource_groups_capacity_check CHECK (capacity_units BETWEEN 1 AND 65536),
    CONSTRAINT resource_groups_device_check CHECK (
        (machine_id = '' AND gpu_device IS NULL) OR
        (machine_id <> '' AND gpu_device IS NOT NULL AND gpu_device >= 0)
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS resource_groups_machine_gpu
    ON model_gateway.resource_groups (machine_id, gpu_device);

-- 已回填的组以组表为准，重跑迁移不能覆盖运维改过的容量。
DO $$
DECLARE conflict_group text;
BEGIN
    SELECT i.resource_group INTO conflict_group
    FROM model_gateway.model_instances i
    WHERE i.deleted_at IS NULL AND NOT EXISTS (
        SELECT 1 FROM model_gateway.resource_groups g WHERE g.resource_group = i.resource_group
    )
    GROUP BY i.resource_group
    HAVING min(i.group_capacity_units) <> max(i.group_capacity_units)
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'Conflicting instance capacities in resource group %', conflict_group;
    END IF;
END $$;

-- 仅在全组成员明确上报同一机器和卡号时绑定，历史共享组保留原组名。
WITH members AS (
    SELECT i.resource_group, i.group_capacity_units, i.machine_id,
        CASE WHEN i.machine_id <> '' AND i.machine_id = btrim(i.machine_id) AND jsonb_typeof(i.gpu_devices) = 'array'
            THEN CASE WHEN jsonb_array_length(i.gpu_devices) = 1 THEN (i.gpu_devices ->> 0)::bigint END
        END AS gpu_device
    FROM model_gateway.model_instances i
    WHERE i.deleted_at IS NULL AND NOT EXISTS (
        SELECT 1 FROM model_gateway.resource_groups g WHERE g.resource_group = i.resource_group
    )
), grouped AS (
    SELECT resource_group, min(group_capacity_units) AS capacity_units, min(machine_id) AS machine_id,
        CASE WHEN count(gpu_device) = count(*) AND min(gpu_device) = max(gpu_device)
            AND min(machine_id) = max(machine_id) THEN min(gpu_device) END AS gpu_device
    FROM members GROUP BY resource_group
)
INSERT INTO model_gateway.resource_groups (resource_group, machine_id, gpu_device, capacity_units)
SELECT resource_group, CASE WHEN gpu_device IS NOT NULL THEN machine_id ELSE '' END, gpu_device, capacity_units
FROM grouped;

COMMIT;
