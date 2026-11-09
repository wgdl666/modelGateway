package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ModelInstance 由部署端登记制品和资源组，网关主动探测。占用从请求账本汇总，重注册不会清零。
type ModelInstance struct{ ent.Schema }

func (ModelInstance) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("instance_id").SchemaType(map[string]string{dialect.Postgres: "text"}).Immutable(),
		field.Text("model_key").Immutable(),
		field.Text("capability").Immutable(),
		field.Text("endpoint").Immutable(),
		field.Text("health_url").Immutable(),
		field.Text("artifact_digest").Immutable(),
		field.Text("resource_group").Immutable(),
		field.Int("group_capacity_units").Positive().Immutable(),
		field.Int("capacity_units").Positive().Immutable(),
		field.Text("machine_id").Default("").Immutable(),
		field.JSON("gpu_devices", []int{}).Optional().Immutable(),
		field.Bool("ready").Default(false),
		field.Time("health_checked_at").Default(func() time.Time { return time.Unix(0, 0).UTC() }),
		field.Bool("draining").Default(false),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (ModelInstance) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "model_instances", Checks: map[string]string{
		"model_instances_capacity_check": "capacity_units > 0 AND group_capacity_units > 0",
	}}}
}

func (ModelInstance) Indexes() []ent.Index {
	return []ent.Index{index.Fields("model_key", "capability"), index.Fields("resource_group")}
}
