package ent

// 行锁用于跨副本容量预留与终态提交；不生成表关系或数据库外键。
//go:generate go run -mod=mod entgo.io/ent/cmd/ent generate --feature sql/lock ./schema
