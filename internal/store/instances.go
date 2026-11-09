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

// 排空实例退出探活集合，已派发任务仍按账本中的地址回查。
func (s *Store) ProbeInstances(ctx context.Context) ([]*ent.ModelInstance, error) {
	return s.db.ModelInstance.Query().Where(modelinstance.DrainingEQ(false)).All(ctx)
}

func (s *Store) Register(ctx context.Context, spec protocol.Instance) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		old, err := tx.ModelInstance.Get(ctx, spec.ID)
		if err == nil {
			// 相同部署重复注册是幂等操作，不改排空状态、健康快照或任务账本。
			if old.ModelKey == spec.ModelKey && old.Capability == spec.Capability && old.Endpoint == spec.Endpoint &&
				old.HealthURL == spec.HealthURL && old.ArtifactDigest == spec.Artifact && old.ResourceGroup == spec.ResourceGroup &&
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
			modelinstance.ResourceGroupEQ(spec.ResourceGroup),
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
			modelinstance.ModelKeyEQ(spec.ModelKey), modelinstance.CapabilityEQ(spec.Capability),
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
			SetResourceGroup(spec.ResourceGroup).SetGroupCapacityUnits(spec.GroupCapacity).
			SetCapacityUnits(spec.Capacity).SetMachineID(spec.MachineID).SetGpuDevices(spec.GPUDevices).Exec(ctx)
	})
}

func (s *Store) Drain(ctx context.Context, id string, draining bool) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		instance, err := tx.ModelInstance.Get(ctx, id)
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
				modelinstance.DrainingEQ(false),
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

func (s *Store) RecordProbe(ctx context.Context, id string, checked time.Time, ready bool) error {
	// 并行探针的响应可能乱序，较早样本不能覆盖较新的可用性判断。
	_, err := s.db.ModelInstance.Update().Where(
		modelinstance.IDEQ(id), modelinstance.HealthCheckedAtLT(checked),
	).SetHealthCheckedAt(checked).SetReady(ready).Save(ctx)
	return err
}
