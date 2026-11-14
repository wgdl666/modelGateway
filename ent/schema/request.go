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

// Request 保存请求状态和容量占用。失败任务可能仍在 GPU 上运行，释放槽位需要单独的终态证据。
type Request struct{ ent.Schema }

func (Request) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("request_id").SchemaType(map[string]string{dialect.Postgres: "text"}).Immutable(),
		// 模型键固定本次执行的业务，也用于迁移后回查已受理任务。
		field.Text("model_key").Immutable(),
		// 协议随身份固定，已有请求不随登记变化改用另一种执行方式。
		field.Text("protocol").Immutable(),
		// 查询链路只需要 Trace ID，避免 Ops 列表读取包含图片的请求正文。
		field.Text("trace_id").Default("").Immutable(),
		// 凭据归属随受理冻结，历史记录不保存明文 Token。
		field.String("api_token_id").Default("").Immutable(),
		field.JSON("request_payload", json.RawMessage{}).Immutable(),
		// 派生信息，不参与受理、派发和幂等。输入清单这类展示数据放在这里，避免每项加一列。
		// 旧副本受理的请求没有输入清单，列表省略 inputs，需读详情；outputs 仍从结果生成。
		field.JSON("extra", json.RawMessage{}).Optional().Immutable(),
		field.Int64("payload_bytes").Positive().Immutable(),
		field.Int("units").Positive().Immutable(),
		field.Enum("state").Values("queued", "processing", "succeeded", "failed").Default("queued"),
		// 仅异步任务显式写投递时间；同步请求不得因数据库默认值再次进入 MQ。
		field.Time("queue_publish_at").Optional().Nillable(),
		field.Bool("slot_reserved").Default(false),
		field.Text("instance_id").Default(""),
		field.Text("resource_group").Default(""),
		// 派发时冻结全部物理资源组，多卡占用不能随着实例登记后续变化而漂移。
		field.JSON("resource_groups", []string{}).Default([]string{}),
		field.Text("endpoint").Default(""),
		field.Text("artifact_digest").Default(""),
		field.Text("deployment_id").Default(""),
		field.Text("boot_id").Default(""),
		field.Text("backend_task_id").Default(""),
		// Counts committed dispatch reservations; legacy zero means unrecorded.
		field.Int("dispatch_attempts").Default(0).NonNegative(),
		field.Text("dispatch_retry_reason").Default(""),
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
		index.Fields("model_key", "created_at", "id").Annotations(entsql.IndexWhere("state = 'queued'")),
		index.Fields("queue_publish_at", "id").Annotations(entsql.IndexWhere("state = 'queued' AND queue_publish_at IS NOT NULL")),
		index.Fields("deadline_at").Annotations(entsql.IndexWhere("state = 'queued'")),
		index.Fields("resource_group", "instance_id").Annotations(entsql.IndexWhere("slot_reserved")),
		index.Fields("next_poll_at").Annotations(entsql.IndexWhere("slot_reserved")),
		index.Fields("created_at", "id"),
		index.Fields("trace_id").Annotations(entsql.IndexWhere("trace_id <> ''")),
	}
}
