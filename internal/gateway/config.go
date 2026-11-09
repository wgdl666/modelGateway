package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"time"
)

type Config struct {
	DatabaseURL       string         `json:"database_url"`
	Listen            string         `json:"listen"`
	APIToken          string         `json:"api_token"`
	WorkerToken       string         `json:"worker_token"`
	AdminToken        string         `json:"admin_token"`
	BackendToken      string         `json:"backend_token"`
	AllowedNetworks   []string       `json:"backend_cidrs"`
	AllowedPorts      []string       `json:"backend_ports"`
	BackendTimeout    time.Duration  `json:"-"`
	PollInterval      time.Duration  `json:"-"`
	MaxBodyBytes      int64          `json:"max_body_bytes"`
	MaxWaiting        int            `json:"max_waiting"`
	MaxWaitingBytes   int64          `json:"max_waiting_bytes"`
	UnitsByCapability map[string]int `json:"units_by_capability"`
}

// 排队和首次模型调用共用 backend_timeout，入队后截止时间固定。
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	wire := struct {
		*Config
		BackendTimeout string `json:"backend_timeout"`
		PollInterval   string `json:"poll_interval"`
	}{Config: &c}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return c, errors.New("invalid gateway configuration JSON")
	}
	for target, value := range map[*time.Duration]string{&c.BackendTimeout: wire.BackendTimeout, &c.PollInterval: wire.PollInterval} {
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

type runtimeConfig struct {
	Config
	networks []*net.IPNet
	ports    map[string]bool
}

func prepareConfig(c Config) (*runtimeConfig, error) {
	if len(c.APIToken) < 24 || len(c.WorkerToken) < 24 || len(c.AdminToken) < 24 || c.APIToken == c.WorkerToken || c.APIToken == c.AdminToken || c.AdminToken == c.WorkerToken {
		return nil, errors.New("three distinct tokens of at least 24 characters required")
	}
	if c.BackendTimeout == 0 {
		c.BackendTimeout = 120 * time.Second
	}
	if c.PollInterval == 0 {
		c.PollInterval = time.Second
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 64 << 20
	}
	if c.MaxWaiting == 0 {
		c.MaxWaiting = 64
	}
	if c.MaxWaitingBytes == 0 {
		c.MaxWaitingBytes = 128 << 20
	}
	if c.BackendTimeout < 0 || c.PollInterval < 0 || c.MaxBodyBytes < 0 || c.MaxWaiting < 0 || c.MaxWaitingBytes < 0 {
		return nil, errors.New("queue limits must be positive")
	}
	for capability, units := range c.UnitsByCapability {
		if !slices.Contains([]string{"cloth_gen", "garment_extraction", "vton", "body_model", "video"}, capability) || units < 1 || units > 65536 {
			return nil, errors.New("invalid capacity units by capability")
		}
	}
	state := &runtimeConfig{Config: c, ports: map[string]bool{}}
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
	return state, nil
}

type runtimeKey struct{}

func requestConfig(ctx context.Context) *runtimeConfig {
	return ctx.Value(runtimeKey{}).(*runtimeConfig)
}

func (s *Server) config() *runtimeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// 活跃队列归固定 PG 所有，切换配置不能让排队或待核对任务留在旧库。
func (s *Server) Reload(_ context.Context, c Config) error {
	next, err := prepareConfig(c)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if next.DatabaseURL != s.current.DatabaseURL {
		return errors.New("changing database_url requires draining and restarting the gateway")
	}
	s.current = next
	return nil
}
