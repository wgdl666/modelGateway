package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Request 通过 instance_id 逻辑关联实例，保留历史归属且不生成外键或级联删除。
type Request struct{ ent.Schema }

func (Request) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("request_id").SchemaType(map[string]string{dialect.Postgres: "text"}).Immutable(),
		field.Text("model_key").Immutable(),
		field.JSON("request_payload", json.RawMessage{}).Immutable(),
		field.Text("instance_id"),
		field.Text("backend_task_id").Optional().Nillable(),
		field.Int("reserved_units").Positive().SchemaType(map[string]string{dialect.Postgres: "integer"}).Immutable(),
		field.Enum("status").Values("reserved", "dispatching", "running", "succeeded", "failed", "unknown").Default("reserved").SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.JSON("result", json.RawMessage{}).Optional(),
		field.Time("created_at").Default(time.Now).Immutable().Annotations(entsql.DefaultExpr("CURRENT_TIMESTAMP")),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).Annotations(entsql.DefaultExpr("CURRENT_TIMESTAMP")),
	}
}

func (Request) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "requests", Checks: map[string]string{
		"requests_units_check":  "reserved_units > 0",
		"requests_status_check": "status IN ('reserved','dispatching','running','succeeded','failed','unknown')",
	}}}
}

func (Request) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("instance_id").StorageKey("requests_instance_idx"),
		index.Fields("updated_at").StorageKey("requests_pending_idx").Annotations(entsql.IndexWhere("status IN ('reserved','dispatching','running','unknown')")),
	}
}
