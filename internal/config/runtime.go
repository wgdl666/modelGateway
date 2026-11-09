package config

import (
	"encoding/json"
	"errors"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type Config struct {
	DatabaseURL       string         `json:"database_url"`
	RabbitMQURL       string         `json:"-"`
	RabbitMQQueue     string         `json:"rabbitmq_queue"`
	Listen            string         `json:"listen"`
	APIToken          string         `json:"api_token"`
	AdminToken        string         `json:"admin_token"`
	BackendToken      string         `json:"backend_token"`
	AllowedNetworks   []string       `json:"backend_cidrs"`
	AllowedPorts      []string       `json:"backend_ports"`
	BackendTimeout    time.Duration  `json:"-"`
	PollInterval      time.Duration  `json:"-"`
	MaxBodyBytes      int64          `json:"max_body_bytes"`
	MaxWaiting        int            `json:"max_waiting"`
	UnitsByCapability map[string]int `json:"units_by_capability"`

	NativeBackends         map[string]NativeBackend `json:"native_backends"`
	NativeResults          NativeResults            `json:"native_results"`
	NativeExecutionTimeout time.Duration            `json:"-"`
}

// 入队固定上游等待截止时间，原生模型的执行预算从领取任务时起算。
func Parse(raw []byte) (Config, error) {
	var c Config
	wire := struct {
		*Config
		BackendTimeout         string `json:"backend_timeout"`
		NativeExecutionTimeout string `json:"native_execution_timeout"`
		PollInterval           string `json:"poll_interval"`
	}{Config: &c}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return c, errors.New("invalid gateway configuration JSON")
	}
	// 数据库和 MQ 凭据由 Secret 注入，配置中心只保存队列名等非密钥设置。
	for key, field := range map[string]*string{
		"MODEL_GATEWAY_DATABASE_URL":  &c.DatabaseURL,
		"MODEL_GATEWAY_RABBITMQ_URL":  &c.RabbitMQURL,
		"MODEL_GATEWAY_API_TOKEN":     &c.APIToken,
		"MODEL_GATEWAY_ADMIN_TOKEN":   &c.AdminToken,
		"MODEL_GATEWAY_BACKEND_TOKEN": &c.BackendToken,
	} {
		if value, ok := os.LookupEnv(key); ok {
			*field = value
		}
	}
	for target, value := range map[*time.Duration]string{
		&c.BackendTimeout: wire.BackendTimeout, &c.NativeExecutionTimeout: wire.NativeExecutionTimeout,
		&c.PollInterval: wire.PollInterval,
	} {
		if value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				return c, errors.New("invalid gateway configuration duration")
			}
			*target = duration
		}
	}
	if c.DatabaseURL == "" {
		return c, errors.New("database_url is required")
	}
	if c.Listen == "" {
		c.Listen = ":18580"
	}
	return c, nil
}

type Snapshot struct {
	Config
	networks []*net.IPNet
	ports    map[string]bool
}

func prepareConfig(c Config) (*Snapshot, error) {
	// 业务调用方与管理员使用独立凭据，避免业务凭据具备实例管理权限。
	if len(c.APIToken) < 24 || len(c.AdminToken) < 24 || c.APIToken == c.AdminToken {
		return nil, errors.New("distinct API and admin tokens of at least 24 characters required")
	}
	if c.BackendTimeout == 0 {
		c.BackendTimeout = 120 * time.Second
	}
	if c.NativeExecutionTimeout == 0 {
		c.NativeExecutionTimeout = 180 * time.Second
	}
	if c.NativeExecutionTimeout < 0 {
		return nil, errors.New("native_execution_timeout must be positive")
	}
	if c.PollInterval == 0 {
		c.PollInterval = time.Second
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 64 << 20
	}
	if c.MaxWaiting == 0 {
		c.MaxWaiting = 128
	}
	if c.BackendTimeout < 0 || c.PollInterval < 0 || c.MaxBodyBytes < 0 || c.MaxWaiting < 0 {
		return nil, errors.New("queue limits must be positive")
	}
	for capability, units := range c.UnitsByCapability {
		if !slices.Contains([]string{"cloth_gen", "garment_extraction", "vton", "body_model", "video"}, capability) || units < 1 || units > 65536 {
			return nil, errors.New("invalid capacity units by capability")
		}
	}
	state := &Snapshot{Config: c, ports: map[string]bool{}}
	for _, cidr := range c.AllowedNetworks {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, errors.New("invalid backend CIDR")
		}
		state.networks = append(state.networks, network)
	}
	for _, port := range c.AllowedPorts {
		state.ports[port] = true
	}
	if len(state.networks) == 0 || len(state.ports) == 0 {
		return nil, errors.New("explicit backend network and port allowlists required")
	}
	if err := state.validateNative(); err != nil {
		return nil, err
	}
	return state, nil
}

// Runtime 让 HTTP 和后台任务共享一次配置更新，各自保留开始处理时读到的快照。
type Runtime struct {
	mu      sync.RWMutex
	current *Snapshot
}

func NewRuntime(c Config) (*Runtime, error) {
	state, err := prepareConfig(c)
	if err != nil {
		return nil, err
	}
	return &Runtime{current: state}, nil
}

func (r *Runtime) Current() *Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// 活跃队列归固定 PG 所有，切换配置不能让排队或待核对任务留在旧库。
func (r *Runtime) Reload(c Config) error {
	next, err := prepareConfig(c)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if next.DatabaseURL != r.current.DatabaseURL {
		return errors.New("changing database_url requires draining and restarting the gateway")
	}
	// 发布与消费必须指向同一队列，切换队列要先处理完旧队列的任务。
	if next.RabbitMQURL != r.current.RabbitMQURL || next.RabbitMQQueue != r.current.RabbitMQQueue {
		return errors.New("changing RabbitMQ requires draining and restarting the gateway")
	}
	// 已派发的请求按原地址回查，协议、视频目标和结果桶都归本次部署所有。
	if !maps.Equal(next.NativeBackends, r.current.NativeBackends) || next.NativeResults != r.current.NativeResults {
		return errors.New("changing native backends or result storage requires draining and restarting the gateway")
	}
	r.current = next
	return nil
}

// 注册和实际出站都限制到允许的字面 IP 与端口，健康地址可带明确路径。
func (s *Snapshot) ValidateEndpoint(endpoint string, health bool) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || (!health && strings.Trim(u.Path, "/") != "") {
		return errors.New("invalid endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	port := u.Port()
	if port == "" && u.Scheme == "https" {
		port = "443"
	} else if port == "" {
		port = "80"
	}
	if ip == nil || !s.ports[port] {
		return errors.New("endpoint is outside the backend allowlist")
	}
	for _, network := range s.networks {
		if network.Contains(ip) {
			return nil
		}
	}
	return errors.New("endpoint is outside the backend allowlist")
}
