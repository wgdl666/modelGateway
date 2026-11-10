package store

import (
	"context"
	"errors"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/resourcegroup"
)

var (
	ErrResourceGroupNotFound       = errors.New("resource group must be created before registering instances")
	ErrResourceGroupExists         = errors.New("resource group name or physical GPU already exists")
	ErrResourceMachineNotFound     = errors.New("machine has no registered GPU resource groups")
	ErrResourceGroupDeviceMismatch = errors.New("deployment machine and GPU must match the resource group")
	ErrLegacyResourceGroup         = errors.New("a legacy group without a GPU binding can only keep its existing deployments")
)

type ResourceGroupView struct {
	*ent.ResourceGroup
	InstanceCount int `json:"instance_count"`
	ReservedUnits int `json:"reserved_units"`
}

func (s *Store) CreateResourceGroup(ctx context.Context, id, machineID string, gpu, capacity int) (*ent.ResourceGroup, error) {
	// 唯一索引阻止并发创建时同一物理卡取得两份额度。
	group, err := s.db.ResourceGroup.Create().SetID(id).SetMachineID(machineID).
		SetGpuDevice(gpu).SetCapacityUnits(capacity).Save(ctx)
	if ent.IsConstraintError(err) {
		return nil, ErrResourceGroupExists
	}
	return group, err
}

func (s *Store) UpdateResourceGroupCapacity(ctx context.Context, id string, capacity int) (*ent.ResourceGroup, error) {
	// 更新只修改上限。派发事务锁同一组记录，在途槽位继续由请求账本持有。
	group, err := s.db.ResourceGroup.UpdateOneID(id).SetCapacityUnits(capacity).Save(ctx)
	if ent.IsNotFound(err) {
		return nil, ErrResourceGroupNotFound
	}
	return group, err
}

func (s *Store) RenameResourceGroupMachine(ctx context.Context, machineID, newMachineID string) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		// 与派发按相同顺序锁定该机器的组，原组标识仍用于占槽和历史请求。
		ids, err := tx.ResourceGroup.Query().Where(resourcegroup.MachineIDEQ(machineID)).
			Order(resourcegroup.ByID()).ForUpdate().IDs(ctx)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return ErrResourceMachineNotFound
		}
		if machineID == newMachineID {
			return nil
		}
		if err := tx.ResourceGroup.Update().Where(resourcegroup.IDIn(ids...)).SetMachineID(newMachineID).Exec(ctx); err != nil {
			if ent.IsConstraintError(err) {
				return ErrResourceGroupExists
			}
			return err
		}
		// 登记校验要求机器标识一致，多张卡及其现有实例一起提交。
		return tx.ModelInstance.Update().
			Where(modelinstance.ResourceGroupIn(ids...), modelinstance.DeletedAtIsNil()).
			SetMachineID(newMachineID).Exec(ctx)
	})
}

func (s *Store) ListResourceGroups(ctx context.Context) ([]ResourceGroupView, error) {
	groups, err := s.db.ResourceGroup.Query().Order(resourcegroup.ByID()).All(ctx)
	if err != nil {
		return nil, err
	}
	var counts []struct {
		Group string `json:"resource_group"`
		Count int    `json:"count"`
	}
	if err := s.db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil()).
		GroupBy(modelinstance.FieldResourceGroup).Aggregate(ent.Count()).Scan(ctx, &counts); err != nil {
		return nil, err
	}
	holds, err := reserved(ctx, s.db.Request)
	if err != nil {
		return nil, err
	}
	instances, units := make(map[string]int), make(map[string]int)
	for _, row := range counts {
		instances[row.Group] = row.Count
	}
	for _, hold := range holds {
		units[hold.ResourceGroup] += hold.Units
	}
	// 从组表起查以保留空组，占用口径和派发共用账本。
	views := make([]ResourceGroupView, 0, len(groups))
	for _, group := range groups {
		views = append(views, ResourceGroupView{
			ResourceGroup: group, InstanceCount: instances[group.ID], ReservedUnits: units[group.ID],
		})
	}
	return views, nil
}

func groupCapacities(ctx context.Context, query *ent.ResourceGroupQuery) (map[string]int, error) {
	groups, err := query.Select(resourcegroup.FieldID, resourcegroup.FieldCapacityUnits).All(ctx)
	if err != nil {
		return nil, err
	}
	limits := make(map[string]int, len(groups))
	for _, group := range groups {
		limits[group.ID] = group.CapacityUnits
	}
	return limits, nil
}
