package store

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

var (
	ErrInstanceNotEditable = errors.New("pause the instance and wait for all reserved requests before editing")
	ErrInstanceChanged     = errors.New("instance settings changed; reopen the editor")
	ErrInstanceAddress     = errors.New("endpoint and health URL must match the configured backend protocol")
	ErrInstanceDestination = errors.New("the destination endpoint belongs to a different resource group or artifact")
)

func (s *Store) EditInstance(ctx context.Context, cfg config.Config, id string, settings protocol.InstanceSettings) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		row, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(id), modelinstance.DeletedAtIsNil()).ForUpdate().Only(ctx)
		if err != nil {
			return err
		}
		// 同步登记的设备列表来自实际部署，队列编辑器只能携带一个组。
		if protocol.SynchronousKind(row.Protocol) != "" {
			return ErrInstanceAddress
		}
		// 返回丢失后的原样重试不会重置探活或影响已恢复的接单状态。
		if row.Endpoint == settings.Endpoint && row.HealthURL == settings.HealthURL &&
			row.ResourceGroup == settings.ResourceGroup && row.CapacityUnits == settings.Capacity {
			return nil
		}
		if !row.Draining {
			return ErrInstanceNotEditable
		}
		if row.UpdatedAt.UTC().Format(time.RFC3339Nano) != settings.ConfigRevision {
			return ErrInstanceChanged
		}
		// 与派单争用实例行锁，失败但还占槽的请求也必须先释放。
		held, err := tx.Request.Query().Where(request.InstanceIDEQ(id), request.SlotReservedEQ(true)).Exist(ctx)
		if err != nil {
			return err
		}
		if held {
			return ErrInstanceNotEditable
		}
		if !cfg.AcceptsInstanceAddress(row.ModelKey, row.Protocol, settings.Endpoint, settings.HealthURL) {
			return ErrInstanceAddress
		}
		group, err := tx.ResourceGroup.Get(ctx, settings.ResourceGroup)
		if ent.IsNotFound(err) {
			return ErrResourceGroupNotFound
		}
		if err != nil {
			return err
		}
		machine, devices := row.MachineID, row.GpuDevices
		if group.GpuDevice != nil {
			matchesDevice := machine == group.MachineID && slices.Equal(devices, []int{*group.GpuDevice})
			// 同一个执行地址不能在已有物理设备绑定时改报另一张卡。
			if settings.Endpoint == row.Endpoint && machine != "" && len(devices) > 0 && !matchesDevice {
				return ErrResourceGroupDeviceMismatch
			}
			machine, devices = group.MachineID, []int{*group.GpuDevice}
		}
		if !cfg.AcceptsInstanceGPU(row.ModelKey, settings.Endpoint, devices) {
			return ErrResourceGroupDeviceMismatch
		}
		if row.Endpoint != settings.Endpoint || row.ResourceGroup != settings.ResourceGroup {
			peers, err := tx.ModelInstance.Query().Where(
				modelinstance.IDNEQ(id), modelinstance.ModelKeyEQ(row.ModelKey),
				modelinstance.EndpointEQ(settings.Endpoint), modelinstance.DeletedAtIsNil(),
			).All(ctx)
			if err != nil {
				return err
			}
			for _, peer := range peers {
				// 两个未上报的摘要不能证明目标地址和旧登记使用同一制品。
				if peer.ResourceGroup != group.ID || row.ArtifactDigest == "" || peer.ArtifactDigest != row.ArtifactDigest {
					return ErrInstanceDestination
				}
			}
			if group.GpuDevice == nil && len(peers) == 0 {
				return ErrLegacyResourceGroup
			}
		}
		return tx.ModelInstance.UpdateOne(row).
			SetEndpoint(settings.Endpoint).SetHealthURL(settings.HealthURL).
			SetResourceGroup(group.ID).SetGroupCapacityUnits(group.CapacityUnits).SetCapacityUnits(settings.Capacity).
			SetMachineID(machine).SetGpuDevices(devices).
			SetReady(false).SetHealthCheckedAt(time.Unix(0, 0).UTC()).Exec(ctx)
	})
}
