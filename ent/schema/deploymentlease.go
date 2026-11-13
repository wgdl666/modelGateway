package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"time"
)

// DeploymentLease fences management mutations for one physical resource group.
// Expiry permits controller takeover, never automatic resumption of traffic.
type DeploymentLease struct{ ent.Schema }

func (DeploymentLease) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("resource_group").Immutable(),
		field.String("region"), field.String("worker_id"),
		field.String("operation_id").Default(""), field.String("owner").Default(""),
		field.Int64("epoch").Default(0), field.Time("lease_until").Default(func() time.Time { return time.Unix(0, 0).UTC() }),
	}
}
func (DeploymentLease) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "deployment_leases"}}
}
