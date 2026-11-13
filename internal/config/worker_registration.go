package config

import (
	"errors"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// Only loaded from a regional Secret, not from the public config document.
type WorkerRegistration struct {
	Token    string              `json:"token"`
	Region   string              `json:"region"`
	Bindings []protocol.Instance `json:"bindings"`
}

func (s *Snapshot) validateWorkerRegistrations() error {
	tokens := map[string]bool{s.AdminToken: true, s.APIToken: true, s.ExperimentalToken: true, s.BackendToken: true, s.SubjectPoolAPIKey: true}
	groups := map[string]bool{}
	for worker, grant := range s.WorkerRegistrations {
		if !protocol.ValidInstanceID(worker) || grant.Region == "" || len(grant.Token) < 32 || tokens[grant.Token] || len(grant.Bindings) == 0 || len(grant.Bindings) > 16 {
			return errors.New("invalid worker registration grant")
		}
		tokens[grant.Token] = true
		models := map[string]bool{}
		first := grant.Bindings[0]
		if groups[first.ResourceGroup] {
			return errors.New("physical resource group has multiple worker grants")
		}
		groups[first.ResourceGroup] = true
		for _, b := range grant.Bindings {
			b.ID = "validation"
			if !b.Valid() || b.MachineID == "" || len(b.GPUDevices) != 1 || b.GPUDevices[0] < 0 || b.ResourceGroup != first.ResourceGroup || b.MachineID != first.MachineID || b.GPUDevices[0] != first.GPUDevices[0] || models[b.ModelKey] || protocol.SynchronousKind(b.Protocol) != "" || b.DeploymentID != "" || b.Artifact != "" {
				return errors.New("worker bindings must cover one physical worker and distinct model keys")
			}
			models[b.ModelKey] = true
			target := protocol.InstanceTarget{ModelKey: b.ModelKey, Protocol: b.Protocol, Endpoint: b.Endpoint, HealthURL: b.HealthURL, GPUDevices: b.GPUDevices}
			if err := s.ValidateInstanceTarget(target); err != nil {
				return err
			}
			if !s.AcceptsInstanceGPU(b.ModelKey, b.Endpoint, b.GPUDevices) {
				return errors.New("invalid worker GPU binding")
			}
		}
	}
	return nil
}
