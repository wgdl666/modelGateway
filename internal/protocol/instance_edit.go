package protocol

// InstanceSettings 只接受运维可编辑字段，部署身份仍由实例登记持有。
type InstanceSettings struct {
	Endpoint       string `json:"endpoint"`
	HealthURL      string `json:"health_url"`
	ResourceGroup  string `json:"resource_group"`
	Capacity       int    `json:"capacity_units"`
	ConfigRevision string `json:"config_revision"`
}
