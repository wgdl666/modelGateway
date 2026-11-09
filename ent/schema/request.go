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

// Request 是队列和容量账本。失败任务可能仍在 GPU 上运行，释放槽位需要单独的终态证据。
type Request struct{ ent.Schema }

func (Request) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("request_id").SchemaType(map[string]string{dialect.Postgres: "text"}).Immutable(),
		field.Text("model_key").Immutable(),
		field.Text("capability").Immutable(),
		// 查询链路只需要 Trace ID，避免 Ops 列表读取包含图片的请求正文。
		field.Text("trace_id").Default("").Immutable(),
		field.JSON("request_payload", json.RawMessage{}).Immutable(),
		field.Int64("payload_bytes").Positive().Immutable(),
		field.Int("units").Positive().Immutable(),
		field.Enum("state").Values("queued", "processing", "succeeded", "failed").Default("queued"),
		field.Bool("slot_reserved").Default(false),
		field.Text("instance_id").Default(""),
		field.Text("resource_group").Default(""),
		field.Text("endpoint").Default(""),
		field.Text("artifact_digest").Default(""),
		field.Text("backend_task_id").Default(""),
		field.Time("deadline_at").Immutable(),
		field.Time("started_at").Optional().Nillable(),
		field.Time("finished_at").Optional().Nillable(),
		field.Time("next_poll_at").Optional().Nillable(),
		field.Text("poll_claim").Default(""),
		field.JSON("result", json.RawMessage{}).Optional(),
		field.Text("error_code").Default(""),
		field.Text("error_message").Default(""),
		field.Int("error_status").Default(0),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Request) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "requests", Checks: map[string]string{
		"requests_units_check": "units > 0 AND payload_bytes > 0",
		"requests_state_check": "state IN ('queued','processing','succeeded','failed')",
		"requests_slot_check":  "NOT slot_reserved OR (state IN ('processing','failed') AND instance_id <> '' AND resource_group <> '' AND endpoint <> '')",
	}}}
}

func (Request) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("model_key", "capability", "created_at", "id").Annotations(entsql.IndexWhere("state = 'queued'")),
		index.Fields("deadline_at").Annotations(entsql.IndexWhere("state = 'queued'")),
		index.Fields("resource_group", "instance_id").Annotations(entsql.IndexWhere("slot_reserved")),
		index.Fields("next_poll_at").Annotations(entsql.IndexWhere("slot_reserved")),
		index.Fields("created_at", "id"),
		index.Fields("trace_id").Annotations(entsql.IndexWhere("trace_id <> ''")),
	}
}
