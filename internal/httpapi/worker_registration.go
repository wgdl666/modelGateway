package httpapi

import (
	"context"
	"crypto/subtle"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func (s *Server) workerLifecycle(ctx context.Context, c *app.RequestContext) {
	cfg := requestConfig(ctx)
	worker, action := c.Param("worker"), c.Param("action")
	grant, ok := cfg.WorkerRegistrations[worker]
	if !ok || subtle.ConstantTimeCompare(c.GetHeader("Authorization"), []byte("Bearer "+grant.Token)) != 1 {
		writeError(c, 401, "unauthorized", "invalid worker credential", "")
		return
	}
	if action != "register" && action != "heartbeat" && action != "drain" && action != "unregister" {
		writeError(c, 404, "not_found", "unknown lifecycle action", "")
		return
	}
	var session protocol.WorkerSession
	if _, ok := readBody(c, 16<<10, &session, ""); !ok {
		return
	}
	if session.Region != grant.Region || !protocol.ValidInstanceID(session.DeploymentID) || !protocol.ValidInstanceID(session.BootID) {
		writeError(c, 400, "invalid_worker_session", "invalid deployment, boot or region", "")
		return
	}
	if action != "register" {
		result, err := s.store.WorkerSession(ctx, worker, session, action, grant.Bindings)
		if err != nil {
			storeError(c, err, "")
			return
		}
		writeJSON(c, 200, result)
		return
	}
	domain, err := s.store.DeploymentDomain(ctx)
	if err != nil {
		storeError(c, err, "")
		return
	}
	if domain != session.Region {
		writeError(c, 409, "worker_session_invalid", "worker belongs to another region", "")
		return
	}
	if len(session.Bindings) != len(grant.Bindings) {
		writeError(c, 400, "invalid_bindings", "complete worker binding set is required", "")
		return
	}
	supplied := map[string]string{}
	for _, binding := range session.Bindings {
		if supplied[binding.ModelKey] != "" || !protocol.ValidArtifact(binding.Artifact) {
			writeError(c, 400, "invalid_bindings", "invalid or duplicate binding", "")
			return
		}
		supplied[binding.ModelKey] = binding.Artifact
	}
	specs := make([]protocol.Instance, 0, len(grant.Bindings))
	for _, spec := range grant.Bindings {
		spec.Artifact = supplied[spec.ModelKey]
		if spec.Artifact == "" {
			writeError(c, 400, "invalid_bindings", "model is outside worker grant", "")
			return
		}
		spec.ID = protocol.WorkerInstanceID(worker, session.DeploymentID, session.BootID, spec.ModelKey)
		spec.DeploymentID = session.DeploymentID
		target := protocol.InstanceTarget{ModelKey: spec.ModelKey, Protocol: spec.Protocol, Endpoint: spec.Endpoint, HealthURL: spec.HealthURL, GPUDevices: spec.GPUDevices, DeploymentID: spec.DeploymentID, ArtifactDigest: spec.Artifact, BootID: session.BootID, WorkerID: worker}
		health, err := s.probe(ctx, cfg, target)
		if err != nil || !health.Ready || health.BootID != session.BootID || health.DeploymentID != session.DeploymentID || health.ArtifactDigest != spec.Artifact || health.Region != session.Region || health.WorkerID != worker {
			writeError(c, 503, "worker_not_ready", "loaded worker identity could not be verified", "")
			return
		}
		specs = append(specs, spec)
	}
	result, err := s.store.RegisterWorker(ctx, worker, session, specs)
	if err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, result)
}
