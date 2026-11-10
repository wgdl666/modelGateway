package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func (s *Server) listResourceGroups(ctx context.Context, c *app.RequestContext) {
	groups, err := s.store.ListResourceGroups(ctx)
	if err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, http.StatusOK, groups)
}

func (s *Server) createResourceGroup(ctx context.Context, c *app.RequestContext) {
	var in struct {
		ID        string `json:"id"`
		MachineID string `json:"machine_id"`
		GPUDevice *int   `json:"gpu_device"`
		Capacity  int    `json:"capacity_units"`
	}
	if _, ok := readBody(c, 4096, &in, ""); !ok {
		return
	}
	// GPU 0 是有效卡号，缺少卡号不能当作 GPU 0。
	if !protocol.ValidResourceGroupID(in.ID) || in.ID != strings.TrimSpace(in.ID) || !validResourceGroupMachineID(in.MachineID) ||
		in.GPUDevice == nil || *in.GPUDevice < 0 ||
		in.Capacity < 1 || in.Capacity > protocol.MaxCapacityUnits {
		writeError(c, http.StatusBadRequest, "invalid_resource_group", "provide a group ID, machine, physical GPU and positive capacity", "")
		return
	}
	group, err := s.store.CreateResourceGroup(ctx, in.ID, in.MachineID, *in.GPUDevice, in.Capacity)
	if err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, http.StatusCreated, group)
}

func validResourceGroupMachineID(machineID string) bool {
	return machineID != "" && len(machineID) <= 256 && machineID == strings.TrimSpace(machineID)
}

func (s *Server) updateResourceGroup(ctx context.Context, c *app.RequestContext) {
	var in struct {
		ID       string `json:"id"`
		Capacity int    `json:"capacity_units"`
	}
	if _, ok := readBody(c, 4096, &in, ""); !ok {
		return
	}
	// 单卡编辑只调整容量，机器标识由机器层统一更正。
	if !protocol.ValidResourceGroupID(in.ID) || in.Capacity < 1 || in.Capacity > protocol.MaxCapacityUnits {
		writeError(c, http.StatusBadRequest, "invalid_resource_group", "provide a group ID and positive capacity", "")
		return
	}
	group, err := s.store.UpdateResourceGroupCapacity(ctx, in.ID, in.Capacity)
	if err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, http.StatusOK, group)
}

func (s *Server) renameResourceGroupMachine(ctx context.Context, c *app.RequestContext) {
	var in struct {
		MachineID    string `json:"machine_id"`
		NewMachineID string `json:"new_machine_id"`
	}
	if _, ok := readBody(c, 4096, &in, ""); !ok {
		return
	}
	// 旧标识定位整台机器，历史组缺少机器归属，不能通过空标识匹配。
	if !validResourceGroupMachineID(in.MachineID) || !validResourceGroupMachineID(in.NewMachineID) {
		writeError(c, http.StatusBadRequest, "invalid_resource_machine", "provide the current and new machine identifiers", "")
		return
	}
	if err := s.store.RenameResourceGroupMachine(ctx, in.MachineID, in.NewMachineID); err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, http.StatusOK, map[string]string{"machine_id": in.NewMachineID})
}
