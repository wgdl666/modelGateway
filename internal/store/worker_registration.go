package store

import (
	"context"
	"entgo.io/ent/dialect/sql"
	"errors"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/deploymentdomain"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"slices"
	"time"
)

var ErrWorkerSession = errors.New("worker session is expired, retired or mismatched")

const WorkerLeaseSeconds = 60

func currentWorkerLease(s *sql.Selector) {
	s.Where(sql.ExprP("(worker_id = '' OR (lease_expires_at > clock_timestamp() AND NOT lifecycle_draining))"))
}

type WorkerLease struct {
	LeaseSeconds     int       `json:"lease_seconds"`
	LeaseExpiresAt   time.Time `json:"lease_expires_at"`
	Draining         bool      `json:"draining"`
	ReservedRequests int       `json:"reserved_requests"`
	Retired          bool      `json:"retired"`
}

func regionalClock(ctx context.Context, tx *ent.Tx, region string) (time.Time, error) {
	domain, err := tx.DeploymentDomain.Get(ctx, 1)
	if err != nil {
		return time.Time{}, err
	}
	if domain.Region != region {
		return time.Time{}, ErrWorkerSession
	}
	seconds, err := tx.DeploymentDomain.Query().Where(deploymentdomain.IDEQ(1)).Aggregate(func(*sql.Selector) string { return "EXTRACT(EPOCH FROM clock_timestamp())" }).Float64(ctx)
	return time.UnixMicro(int64(seconds * 1e6)).UTC(), err
}

// Complete binding sets are changed in one serializable transaction. A new boot
// can never bypass reservations on another model sharing the physical GPU.
func (s *Store) RegisterWorker(ctx context.Context, worker string, session protocol.WorkerSession, specs []protocol.Instance) (WorkerLease, error) {
	var result WorkerLease
	err := s.serialize(ctx, func(tx *ent.Tx) error {
		now, err := regionalClock(ctx, tx, session.Region)
		if err != nil {
			return err
		}
		if len(specs) == 0 {
			return ErrConflict
		}
		group, err := tx.ResourceGroup.Get(ctx, specs[0].ResourceGroup)
		if err != nil {
			return err
		}
		models := map[string]protocol.Instance{}
		for _, spec := range specs {
			if !spec.Valid() || spec.DeploymentID != session.DeploymentID || spec.ID != protocol.WorkerInstanceID(worker, session.DeploymentID, session.BootID, spec.ModelKey) || spec.ResourceGroup != group.ID || group.GpuDevice == nil || spec.MachineID != group.MachineID || len(spec.GPUDevices) != 1 || spec.GPUDevices[0] != *group.GpuDevice || models[spec.ModelKey].ID != "" {
				return ErrConflict
			}
			models[spec.ModelKey] = spec
			conflict, err := tx.ModelInstance.Query().Where(modelinstance.ModelKeyEQ(spec.ModelKey), modelinstance.ProtocolNEQ(spec.Protocol)).Exist(ctx)
			if err != nil {
				return err
			}
			if conflict {
				return ErrConflict
			}
		}
		existing := 0
		for _, spec := range specs {
			row, err := tx.ModelInstance.Get(ctx, spec.ID)
			if ent.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if row.DeletedAt != nil || row.LifecycleDraining || row.LeaseExpiresAt == nil || !row.LeaseExpiresAt.After(now) {
				return ErrWorkerSession
			}
			if row.WorkerID != worker || row.BootID != session.BootID || row.DeploymentID != session.DeploymentID || row.ArtifactDigest != spec.Artifact || row.ModelKey != spec.ModelKey || row.Protocol != spec.Protocol || row.Endpoint != spec.Endpoint || row.HealthURL != spec.HealthURL || row.ResourceGroup != spec.ResourceGroup || row.MachineID != spec.MachineID || row.CapacityUnits != spec.Capacity || !slices.Equal(row.GpuDevices, spec.GPUDevices) {
				return ErrConflict
			}
			existing++
		}
		if existing != 0 && existing != len(specs) {
			return ErrConflict
		}
		expiry := now.Add(WorkerLeaseSeconds * time.Second)
		result = WorkerLease{LeaseSeconds: WorkerLeaseSeconds, LeaseExpiresAt: expiry}
		if existing == len(specs) {
			count, err := tx.ModelInstance.Query().Where(modelinstance.WorkerIDEQ(worker), modelinstance.DeploymentIDEQ(session.DeploymentID), modelinstance.BootIDEQ(session.BootID), modelinstance.DeletedAtIsNil()).Count(ctx)
			if err != nil {
				return err
			}
			if count != len(specs) {
				return ErrWorkerSession
			}
			for _, spec := range specs {
				if err := tx.ModelInstance.UpdateOneID(spec.ID).SetLeaseExpiresAt(expiry).Exec(ctx); err != nil {
					return err
				}
			}
			return nil
		}
		old, err := tx.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil(), modelinstance.ResourceGroupEQ(group.ID)).All(ctx)
		if err != nil {
			return err
		}
		paused := map[string]bool{}
		// Operator pauses survive a clean unregister followed by a later restart.
		history, err := tx.ModelInstance.Query().Where(modelinstance.ResourceGroupEQ(group.ID), modelinstance.WorkerIDEQ(worker)).Order(ent.Desc(modelinstance.FieldCreatedAt)).All(ctx)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, row := range history {
			if !seen[row.ModelKey] {
				paused[row.ModelKey] = row.Draining
				seen[row.ModelKey] = true
			}
		}
		for _, row := range old {
			if _, ok := models[row.ModelKey]; !ok {
				return ErrConflict
			} // Do not silently discard a shared capability.
			if row.WorkerID == "" {
				if !row.Draining {
					return ErrConflict
				}
			} else if row.WorkerID != worker || (!row.LifecycleDraining && row.LeaseExpiresAt != nil && row.LeaseExpiresAt.After(now)) {
				return ErrConflict
			}
			paused[row.ModelKey] = paused[row.ModelKey] || row.Draining
		}
		holds, err := reserved(ctx, tx.Request)
		if err != nil {
			return err
		}
		for _, h := range holds {
			if slices.Contains(h.ResourceGroups, group.ID) {
				return ErrConflict
			}
		}
		for _, row := range old {
			if err := tx.ModelInstance.UpdateOneID(row.ID).SetReady(false).SetLifecycleDraining(true).SetDeletedAt(now).Exec(ctx); err != nil {
				return err
			}
		}
		for _, spec := range specs {
			err := tx.ModelInstance.Create().SetID(spec.ID).SetModelKey(spec.ModelKey).SetProtocol(spec.Protocol).
				SetEndpoint(spec.Endpoint).SetHealthURL(spec.HealthURL).SetArtifactDigest(spec.Artifact).SetDeploymentID(session.DeploymentID).
				SetWorkerID(worker).SetBootID(session.BootID).SetLeaseExpiresAt(expiry).
				SetResourceGroup(group.ID).SetGroupCapacityUnits(group.CapacityUnits).SetCapacityUnits(spec.Capacity).
				SetMachineID(spec.MachineID).SetGpuDevices(spec.GPUDevices).SetDraining(paused[spec.ModelKey] || spec.InitiallyDraining).
				SetReady(true).SetHealthCheckedAt(now).Exec(ctx)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

// Heartbeats never clear operator drain or the irreversible process drain flag.
// Expiry revokes renewal, but still permits drain/unregister for cleanup.
func (s *Store) WorkerSession(ctx context.Context, worker string, session protocol.WorkerSession, action string, scope []protocol.Instance) (WorkerLease, error) {
	var result WorkerLease
	err := s.serialize(ctx, func(tx *ent.Tx) error {
		now, err := regionalClock(ctx, tx, session.Region)
		if err != nil {
			return err
		}
		rows, err := tx.ModelInstance.Query().Where(modelinstance.WorkerIDEQ(worker), modelinstance.DeploymentIDEQ(session.DeploymentID), modelinstance.BootIDEQ(session.BootID)).All(ctx)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrWorkerSession
		}
		if action == "heartbeat" {
			if len(rows) != len(scope) {
				return ErrWorkerSession
			}
			for _, row := range rows {
				matched := false
				for _, allowed := range scope {
					if row.ModelKey == allowed.ModelKey && row.Protocol == allowed.Protocol && row.Endpoint == allowed.Endpoint && row.HealthURL == allowed.HealthURL && row.ResourceGroup == allowed.ResourceGroup && row.MachineID == allowed.MachineID && row.CapacityUnits == allowed.Capacity && slices.Equal(row.GpuDevices, allowed.GPUDevices) {
						matched = true
						break
					}
				}
				if !matched {
					return ErrWorkerSession
				}
			}
		}
		result = WorkerLease{LeaseSeconds: WorkerLeaseSeconds, Retired: true}
		ids := map[string]bool{}
		for _, row := range rows {
			ids[row.ID] = true
			if row.DeletedAt == nil {
				result.Retired = false
			}
		}
		if result.Retired {
			if action == "unregister" {
				return nil
			}
			return ErrWorkerSession
		}
		holds, err := reserved(ctx, tx.Request)
		if err != nil {
			return err
		}
		for _, h := range holds {
			if ids[h.InstanceID] {
				result.ReservedRequests += h.Requests
			}
		}
		for _, row := range rows {
			if row.DeletedAt != nil {
				return ErrWorkerSession
			}
			result.Draining = result.Draining || row.LifecycleDraining
			if action == "heartbeat" && (row.LeaseExpiresAt == nil || !row.LeaseExpiresAt.After(now)) {
				return ErrWorkerSession
			}
		}
		if action == "unregister" && (!result.Draining || result.ReservedRequests > 0) {
			return ErrInstanceNotClearable
		}
		expiry := now.Add(WorkerLeaseSeconds * time.Second)
		for _, row := range rows {
			update := tx.ModelInstance.UpdateOneID(row.ID)
			switch action {
			case "heartbeat":
				update.SetLeaseExpiresAt(expiry)
			case "drain":
				update.SetLifecycleDraining(true).SetReady(false)
			case "unregister":
				update.SetDeletedAt(now).SetReady(false)
			default:
				return ErrConflict
			}
			if err := update.Exec(ctx); err != nil {
				return err
			}
		}
		result.LeaseExpiresAt = expiry
		if action != "heartbeat" && rows[0].LeaseExpiresAt != nil {
			result.LeaseExpiresAt = *rows[0].LeaseExpiresAt
		}
		result.Draining = result.Draining || action == "drain"
		result.Retired = action == "unregister"
		return nil
	})
	return result, err
}
