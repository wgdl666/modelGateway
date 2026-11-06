package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/lib/pq"
)

// ModelInstance 保留现有实例标识及 GPU 数组类型，容量与租约共同决定能否受理请求。
type ModelInstance struct{ ent.Schema }

func (ModelInstance) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("instance_id").SchemaType(map[string]string{dialect.Postgres: "text"}).Immutable(),
		field.Text("model_key").Immutable(),
		field.Text("endpoint").Immutable(),
		field.Text("machine_id").Default("").Immutable(),
		field.Other("gpu_devices", pq.Int64Array{}).SchemaType(map[string]string{dialect.Postgres: "integer[]"}).Immutable(),
		field.Int("capacity_units").Positive().SchemaType(map[string]string{dialect.Postgres: "integer"}),
		field.Int("used_units").NonNegative().Default(0).SchemaType(map[string]string{dialect.Postgres: "integer"}),
		field.Bool("ready").Default(false),
		field.Bool("draining").Default(false),
		field.Time("lease_expires_at"),
		field.Time("created_at").Default(time.Now).Immutable().Annotations(entsql.DefaultExpr("CURRENT_TIMESTAMP")),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).Annotations(entsql.DefaultExpr("CURRENT_TIMESTAMP")),
	}
}

func (ModelInstance) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "model_instances", Checks: map[string]string{
		"model_instances_capacity_check": "capacity_units > 0",
		"model_instances_used_check":     "used_units >= 0 AND used_units <= capacity_units",
	}}}
}

func (ModelInstance) Indexes() []ent.Index {
	return []ent.Index{index.Fields("model_key").StorageKey("instances_model_idx")}
}
