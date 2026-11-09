package store

import (
	"context"
	"slices"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

const probeMaxAge = 9 * time.Second

func instanceHealthy(row *ent.ModelInstance, now time.Time) bool {
	return row.Ready && row.HealthCheckedAt.After(now.Add(-probeMaxAge))
}

// 未探测和未就绪是不同状态；暂停后过期的探活结果按未就绪处理。
func instanceClearable(row *ent.ModelInstance, healthy bool) bool {
	return row.Draining && row.HealthCheckedAt.Unix() > 0 && !healthy
}

// 排空实例退出探活集合，已派发任务仍按账本中的地址回查。
func (s *Store) ProbeInstances(ctx context.Context) ([]*ent.ModelInstance, error) {
	return s.db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil(), modelinstance.DrainingEQ(false)).All(ctx)
}

func (s *Store) Register(ctx context.Context, spec protocol.Instance) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		old, err := tx.ModelInstance.Get(ctx, spec.ID)
		if err == nil {
			// 已清除的 ID 仍归属原部署，注册重试不能让旧登记重新出现。
			if old.DeletedAt != nil {
				return ErrConflict
			}
			// 相同部署重复注册是幂等操作，不改排空状态、健康快照或任务账本。
			if old.ModelKey == spec.ModelKey && old.Capability == spec.Capability && old.Endpoint == spec.Endpoint &&
				old.HealthURL == spec.HealthURL && old.ArtifactDigest == spec.Artifact && old.ResourceGroup == spec.ResourceGroup &&
				old.TargetInstance == spec.TargetInstance &&
				old.GroupCapacityUnits == spec.GroupCapacity && old.CapacityUnits == spec.Capacity &&
				old.MachineID == spec.MachineID && slices.Equal(old.GpuDevices, spec.GPUDevices) {
				return nil
			}
			return ErrConflict
		}
		if !ent.IsNotFound(err) {
			return err
		}
		conflict, err := tx.ModelInstance.Query().Where(
			modelinstance.DeletedAtIsNil(), modelinstance.ResourceGroupEQ(spec.ResourceGroup),
			modelinstance.GroupCapacityUnitsNEQ(spec.GroupCapacity),
		).Exist(ctx)
		if err != nil {
			return err
		}
		if conflict {
			return ErrConflict
		}
		// 同一执行地址的新实例不能绕过仍在执行的旧实例及其资源组。
		sameEndpoint, err := tx.ModelInstance.Query().Where(
			modelinstance.DeletedAtIsNil(), modelinstance.ModelKeyEQ(spec.ModelKey), modelinstance.CapabilityEQ(spec.Capability),
			modelinstance.EndpointEQ(spec.Endpoint),
		).All(ctx)
		if err != nil {
			return err
		}
		for _, instance := range sameEndpoint {
			held, err := tx.Request.Query().Where(request.InstanceIDEQ(instance.ID), request.SlotReservedEQ(true)).Exist(ctx)
			if err != nil {
				return err
			}
			if !instance.Draining || held {
				return ErrConflict
			}
		}
		return tx.ModelInstance.Create().
			SetID(spec.ID).SetModelKey(spec.ModelKey).SetCapability(spec.Capability).
			SetEndpoint(spec.Endpoint).SetHealthURL(spec.HealthURL).SetArtifactDigest(spec.Artifact).
			SetResourceGroup(spec.ResourceGroup).SetTargetInstance(spec.TargetInstance).SetGroupCapacityUnits(spec.GroupCapacity).
			SetCapacityUnits(spec.Capacity).SetMachineID(spec.MachineID).SetGpuDevices(spec.GPUDevices).Exec(ctx)
	})
}

func (s *Store) Drain(ctx context.Context, id string, draining bool) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		instance, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(id), modelinstance.DeletedAtIsNil()).Only(ctx)
		if err != nil {
			return err
		}
		if !draining {
			// 原登记用于追溯历史请求。重新接单时须用当前模型键登记新实例。
			if !protocol.MatchesCapability(instance.ModelKey, instance.Capability) {
				return ErrLegacyRegistration
			}
			conflict, err := tx.ModelInstance.Query().Where(
				modelinstance.IDNEQ(id), modelinstance.ModelKeyEQ(instance.ModelKey),
				modelinstance.CapabilityEQ(instance.Capability), modelinstance.EndpointEQ(instance.Endpoint),
				modelinstance.DeletedAtIsNil(), modelinstance.DrainingEQ(false),
			).Exist(ctx)
			if err != nil {
				return err
			}
			if conflict {
				return ErrConflict
			}
		}
		return tx.ModelInstance.UpdateOneID(id).SetDraining(draining).Exec(ctx)
	})
}

func (s *Store) ClearInstance(ctx context.Context, id string) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		// 与派单争用同一实例行锁，在同一事务确认排空和槽位后才标记删除。
		row, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(id)).ForUpdate().Only(ctx)
		if err != nil {
			return err
		}
		if row.DeletedAt != nil {
			return nil
		}
		now := time.Now().UTC()
		if !instanceClearable(row, instanceHealthy(row, now)) {
			return ErrInstanceNotClearable
		}
		// 已对外失败但后端仍可能在执行的请求也会占槽，不能只判断请求终态。
		held, err := tx.Request.Query().Where(request.InstanceIDEQ(id), request.SlotReservedEQ(true)).Exist(ctx)
		if err != nil {
			return err
		}
		if held {
			return ErrInstanceNotClearable
		}
		return tx.ModelInstance.UpdateOneID(id).SetDeletedAt(now).Exec(ctx)
	})
}

func (s *Store) RecordProbe(ctx context.Context, id string, checked time.Time, ready bool) error {
	// 并行探针的响应可能乱序，较早样本不能覆盖较新的可用性判断。
	_, err := s.db.ModelInstance.Update().Where(
		modelinstance.IDEQ(id), modelinstance.DeletedAtIsNil(), modelinstance.HealthCheckedAtLT(checked),
	).SetHealthCheckedAt(checked).SetReady(ready).Save(ctx)
	return err
}
