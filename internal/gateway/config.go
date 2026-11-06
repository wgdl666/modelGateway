package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DatabaseURL     string        `json:"database_url"`
	Listen          string        `json:"listen"`
	APIToken        string        `json:"api_token"`
	WorkerToken     string        `json:"worker_token"`
	AdminToken      string        `json:"admin_token"`
	BackendToken    string        `json:"backend_token"`
	AllowedNetworks []string      `json:"backend_cidrs"`
	AllowedPorts    []string      `json:"backend_ports"`
	BackendTimeout  time.Duration `json:"-"`
	PollInterval    time.Duration `json:"-"`
	InstanceLease   time.Duration `json:"-"`
	MaxBodyBytes    int64         `json:"max_body_bytes"`
}

// 配置中心使用 120s、5s 等可读时长，内部继续使用标准库的 time.Duration。
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	wire := struct {
		*Config
		BackendTimeout string `json:"backend_timeout"`
		PollInterval   string `json:"poll_interval"`
		InstanceLease  string `json:"instance_lease"`
	}{Config: &c}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return c, errors.New("invalid gateway configuration JSON")
	}
	for target, value := range map[*time.Duration]string{&c.BackendTimeout: wire.BackendTimeout, &c.PollInterval: wire.PollInterval, &c.InstanceLease: wire.InstanceLease} {
		if value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil {
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

type database struct {
	pool  *pgxpool.Pool
	users sync.WaitGroup
}

type runtimeConfig struct {
	Config
	db       *database
	store    *Store
	networks []*net.IPNet
	ports    map[string]bool
}

func prepareConfig(c Config) (*runtimeConfig, error) {
	if len(c.APIToken) < 24 || len(c.WorkerToken) < 24 || len(c.AdminToken) < 24 || c.APIToken == c.WorkerToken || c.APIToken == c.AdminToken || c.AdminToken == c.WorkerToken {
		return nil, errors.New("three distinct tokens of at least 24 characters required")
	}
	if c.BackendTimeout <= 0 {
		c.BackendTimeout = 120 * time.Second
	}
	if c.PollInterval <= 3*time.Second {
		c.PollInterval = 5 * time.Second
	}
	if c.InstanceLease <= 0 {
		c.InstanceLease = 30 * time.Second
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 16 << 20
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

func (s *Server) acquire() (*runtimeConfig, func()) {
	s.mu.RLock()
	state := s.current
	state.db.users.Add(1)
	s.mu.RUnlock()
	return state, state.db.users.Done
}

// 先准备新快照，再一次切换；请求和后台任务持有旧池直到完成，避免换配置破坏已受理任务。
func (s *Server) Reload(ctx context.Context, c Config) error {
	next, err := prepareConfig(c)
	if err != nil {
		return err
	}
	s.mu.RLock()
	old := s.current
	s.mu.RUnlock()
	next.db = old.db
	if c.DatabaseURL != old.DatabaseURL {
		pool, err := pgxpool.New(ctx, c.DatabaseURL)
		if err == nil {
			err = pool.Ping(ctx)
		}
		if err != nil {
			if pool != nil {
				pool.Close()
			}
			return errors.New("cannot connect to configured PostgreSQL")
		}
		next.db = &database{pool: pool}
	}
	next.store = &Store{DB: next.db.pool, Lease: next.InstanceLease}
	s.mu.Lock()
	s.current = next
	s.mu.Unlock()
	if old.db != next.db {
		s.retired.Add(1)
		go func() {
			defer s.retired.Done()
			old.db.users.Wait()
			old.db.pool.Close()
		}()
	}
	select {
	case s.configChanged <- struct{}{}:
	default:
	}
	return nil
}
