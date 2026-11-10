package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ResourceGroup 独占共享容量；历史共享组可能没有可核实的单卡绑定。
type ResourceGroup struct{ ent.Schema }

func (ResourceGroup) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("resource_group").SchemaType(map[string]string{dialect.Postgres: "text"}).Immutable(),
		// 机器层更正标识时，服务层会同步这台机器下的卡和关联实例。
		field.Text("machine_id").Default(""),
		field.Int("gpu_device").Optional().Nillable().NonNegative().Immutable(),
		field.Int("capacity_units").Positive(),
	}
}

func (ResourceGroup) Indexes() []ent.Index {
	return []ent.Index{index.Fields("machine_id", "gpu_device").Unique().StorageKey("resource_groups_machine_gpu")}
}

func (ResourceGroup) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "resource_groups", Checks: map[string]string{
		"resource_groups_capacity_check": "capacity_units BETWEEN 1 AND 65536",
		"resource_groups_device_check":   "(machine_id = '' AND gpu_device IS NULL) OR (machine_id <> '' AND gpu_device IS NOT NULL AND gpu_device >= 0)",
	}}}
}
