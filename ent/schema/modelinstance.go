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
		// 排空后可迁移执行地址，已有请求仍使用派发时写入的地址。
		field.Text("endpoint"),
		field.Text("health_url"),
		field.Text("artifact_digest").Immutable(),
		field.Text("resource_group"),
		// LTX 旧根地址仍使用部署登记的实例目标。
		field.Text("target_instance").Default("").Immutable(),
		// 切换期间旧网关仍读取此列，新网关从资源组表取实时上限。
		field.Int("group_capacity_units").Positive(),
		field.Int("capacity_units").Positive(),
		// 转入物理资源组时由组记录带入设备归属。
		field.Text("machine_id").Default(""),
		field.JSON("gpu_devices", []int{}).Optional(),
		field.Bool("ready").Default(false),
		field.Time("health_checked_at").Default(func() time.Time { return time.Unix(0, 0).UTC() }),
		field.Bool("draining").Default(false),
		// 清除只标记实例登记，保留实例 ID 供历史请求追溯。
		field.Time("deleted_at").Optional().Nillable(),
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
