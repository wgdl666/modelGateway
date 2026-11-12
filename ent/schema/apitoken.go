package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"time"
)

// APIToken 保存各调用方的独立凭据；撤销保留审计元数据，密文不参与列表序列化。
type APIToken struct{ ent.Schema }

func (APIToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Immutable(),
		field.String("name").NotEmpty().Immutable(),
		field.Text("encrypted_token").Sensitive().Immutable(),
		field.String("created_by").NotEmpty().Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(),
	}
}
func (APIToken) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "api_tokens"}}
}
