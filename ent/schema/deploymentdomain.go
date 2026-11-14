package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

type DeploymentDomain struct{ ent.Schema }

func (DeploymentDomain) Fields() []ent.Field {
	return []ent.Field{field.Int("id"), field.String("region").Unique().Immutable()}
}
func (DeploymentDomain) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "deployment_domain", Checks: map[string]string{"deployment_domain_singleton": "id = 1"}}}
}
