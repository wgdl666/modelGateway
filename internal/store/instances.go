package store

import (
	"context"
	"slices"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/ent/resourcegroup"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

const probeMaxAge = 9 * time.Second

func instanceHealthy(row *ent.ModelInstance, now time.Time) bool {
	maxAge := probeMaxAge
	if protocol.SynchronousKind(row.Protocol) != "" {
		// G6 聚合探活在单个副本异常时会跨多批超时，使用同步探针的周期和超时预算。
		maxAge = 35 * time.Second
	}
	return row.Ready && row.HealthCheckedAt.After(now.Add(-maxAge))
}

// 未探测和未就绪是不同状态；暂停后过期的探活结果按未就绪处理。
func instanceClearable(row *ent.ModelInstance, healthy bool) bool {
	return row.Draining && row.HealthCheckedAt.Unix() > 0 && !healthy
}

// 排空实例退出探活集合，已派发任务仍按账本中的地址回查。
func (s *Store) ProbeInstances(ctx context.Context) ([]*ent.ModelInstance, error) {
	return s.db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil(), modelinstance.DrainingEQ(false)).All(ctx)
}

func (s *Store) Register(ctx context.Context, spec protocol.Instance, reportedArtifact string) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		if err := deploymentFence(ctx, tx, spec.ResourceGroup); err != nil {
			return err
		}
		// 同一 key 的协议跨副本及历史登记保持一致，避免新实例接到旧契约请求。
		conflict, err := tx.ModelInstance.Query().Where(modelinstance.ModelKeyEQ(spec.ModelKey), modelinstance.ProtocolNEQ(spec.Protocol)).Exist(ctx)
		if err != nil {
			return err
		}
		if conflict {
			return ErrConflict
		}
		group, err := tx.ResourceGroup.Get(ctx, spec.ResourceGroup)
		if ent.IsNotFound(err) {
			return ErrResourceGroupNotFound
		}
		if err != nil {
			return err
		}
		// 单卡队列实例保持原绑定。多卡 Dream 以首张卡为主组，其他物理卡也必须已登记。
		synchronous := protocol.SynchronousKind(spec.Protocol) != ""
		// 当前 FaceSim 使用 OpenCV CPU 推理，独立逻辑组限制并发，不能伪造 GPU 0 归属。
		cpuOnly := spec.Protocol == protocol.FaceSimProtocol
		if cpuOnly && (group.GpuDevice != nil || len(spec.GPUDevices) != 0) {
			return ErrResourceGroupDeviceMismatch
		}
		if synchronous && !cpuOnly && group.GpuDevice == nil {
			return ErrResourceGroupDeviceMismatch
		}
		if group.GpuDevice != nil && (group.MachineID != spec.MachineID || len(spec.GPUDevices) == 0 ||
			spec.GPUDevices[0] != *group.GpuDevice || (spec.Protocol != protocol.ChatProtocol && len(spec.GPUDevices) != 1)) {
			return ErrResourceGroupDeviceMismatch
		}
		if synchronous && len(spec.GPUDevices) > 1 {
			n, err := tx.ResourceGroup.Query().Where(resourcegroup.MachineIDEQ(spec.MachineID),
				resourcegroup.GpuDeviceIn(spec.GPUDevices...)).Count(ctx)
			if err != nil {
				return err
			}
			if n != len(spec.GPUDevices) {
				return ErrResourceGroupDeviceMismatch
			}
		}
		old, err := tx.ModelInstance.Get(ctx, spec.ID)
		if err == nil {
			// 已清除的 ID 仍归属原部署，注册重试不能让旧登记重新出现。
			if old.DeletedAt != nil {
				return ErrConflict
			}
			// 自动获取摘要的登记重试沿用首次写入的身份，探针变化不能改写历史。
			if old.ModelKey == spec.ModelKey && old.DeploymentID == spec.DeploymentID && old.Protocol == spec.Protocol && old.Endpoint == spec.Endpoint &&
				old.HealthURL == spec.HealthURL && (spec.Artifact == "" || old.ArtifactDigest == spec.Artifact) &&
				old.ResourceGroup == spec.ResourceGroup &&
				old.CapacityUnits == spec.Capacity &&
				old.MachineID == spec.MachineID && slices.Equal(old.GpuDevices, spec.GPUDevices) {
				return nil
			}
			return ErrConflict
		}
		if !ent.IsNotFound(err) {
			return err
		}
		// 同一执行地址的新实例不能绕过仍在执行的旧实例及其资源组。
		sameEndpoint, err := tx.ModelInstance.Query().Where(
			modelinstance.ModelKeyEQ(spec.ModelKey), modelinstance.EndpointEQ(spec.Endpoint),
		).All(ctx)
		if err != nil {
			return err
		}
		knownDeployment := false
		for _, instance := range sameEndpoint {
			knownDeployment = knownDeployment || instance.ResourceGroup == spec.ResourceGroup
			if instance.DeletedAt != nil {
				continue
			}
			held, err := tx.Request.Query().Where(request.InstanceIDEQ(instance.ID), request.SlotReservedEQ(true)).Exist(ctx)
			if err != nil {
				return err
			}
			if !instance.Draining || held {
				return ErrConflict
			}
		}
		// 历史组可以重登原服务地址，新增部署必须使用已绑定物理卡的组。
		if group.GpuDevice == nil && !knownDeployment && !cpuOnly {
			return ErrLegacyResourceGroup
		}
		artifact := spec.Artifact
		if artifact == "" {
			artifact = reportedArtifact
		}
		return tx.ModelInstance.Create().
			SetID(spec.ID).SetModelKey(spec.ModelKey).SetProtocol(spec.Protocol).
			SetEndpoint(spec.Endpoint).SetHealthURL(spec.HealthURL).SetArtifactDigest(artifact).SetDeploymentID(spec.DeploymentID).
			SetResourceGroup(spec.ResourceGroup).SetGroupCapacityUnits(group.CapacityUnits).
			SetCapacityUnits(spec.Capacity).SetMachineID(spec.MachineID).SetGpuDevices(spec.GPUDevices).SetDraining(spec.InitiallyDraining).Exec(ctx)
	})
}

func (s *Store) Drain(ctx context.Context, id string, draining bool) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		instance, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(id), modelinstance.DeletedAtIsNil()).Only(ctx)
		if err != nil {
			return err
		}
		if err := deploymentFence(ctx, tx, instance.ResourceGroup); err != nil {
			return err
		}
		if !draining {
			// 同一模型和地址只能有一条接单登记，恢复旧部署时不能绕过这项限制。
			conflict, err := tx.ModelInstance.Query().Where(
				modelinstance.IDNEQ(id), modelinstance.ModelKeyEQ(instance.ModelKey),
				modelinstance.EndpointEQ(instance.Endpoint),
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
		if err := deploymentFence(ctx, tx, row.ResourceGroup); err != nil {
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
	// 暂停或修改后，先前开始的探针不能写回旧地址的就绪结果。
	_, err := s.db.ModelInstance.Update().Where(
		modelinstance.IDEQ(id), modelinstance.DeletedAtIsNil(),
		modelinstance.UpdatedAtLTE(checked), modelinstance.HealthCheckedAtLT(checked),
	).SetHealthCheckedAt(checked).SetReady(ready).Save(ctx)
	return err
}

// ModelProtocol 从已有登记解析处理方式；实例暂时下线或被清除不抹掉模型契约。
func (s *Store) ModelProtocol(ctx context.Context, model string) (string, error) {
	row, err := s.db.ModelInstance.Query().Where(modelinstance.ModelKeyEQ(model)).Select(modelinstance.FieldProtocol).First(ctx)
	if ent.IsNotFound(err) {
		return "", ErrModelUnregistered
	}
	if err != nil {
		return "", err
	}
	return row.Protocol, nil
}
