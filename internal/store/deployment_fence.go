package store

import (
	"context"
	"errors"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/deploymentlease"
	"time"
)

var ErrDeploymentFenced = errors.New("resource group is controlled by a deployment operation; acquire or resume its current fence")

type DeploymentFence struct {
	Operation, Owner string
	Epoch            int64
}
type deploymentFenceKey struct{}

func WithDeploymentFence(ctx context.Context, fence DeploymentFence) context.Context {
	return context.WithValue(ctx, deploymentFenceKey{}, fence)
}
func deploymentFence(ctx context.Context, tx *ent.Tx, group string) error {
	f, supplied := ctx.Value(deploymentFenceKey{}).(DeploymentFence)
	row, err := tx.DeploymentLease.Query().Where(deploymentlease.IDEQ(group)).ForUpdate().Only(ctx)
	if ent.IsNotFound(err) {
		if supplied {
			return ErrDeploymentFenced
		}
		return nil
	}
	if err != nil {
		return err
	}
	if row.OperationID == "" {
		if supplied {
			return ErrDeploymentFenced
		}
		return nil
	}
	if !supplied || row.OperationID != f.Operation || row.Owner != f.Owner || row.Epoch != f.Epoch || !row.LeaseUntil.After(time.Now().UTC()) {
		return ErrDeploymentFenced
	}
	return nil
}
