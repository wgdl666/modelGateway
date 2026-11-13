package config

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

// SubjectPool is one aggregate reservation identity, never a replica identity.
// Its endpoint is a private authenticated Kong ingress.
type SubjectPool struct {
	Protocol  string `json:"protocol"`
	Endpoint  string `json:"endpoint"`
	HealthURL string `json:"health_url"`
}

func (c Config) SubjectPoolFor(model, kind, endpoint string) (SubjectPool, bool) {
	p, ok := c.SubjectPools[model]
	return p, ok && model == protocol.SegmentSubjectModel && kind == protocol.SegmentSubjectProtocol && p.Protocol == kind && p.Endpoint == endpoint
}
func (c Config) subjectPoolURL(endpoint string) bool {
	for _, p := range c.SubjectPools {
		if endpoint == p.Endpoint || endpoint == p.HealthURL {
			return true
		}
	}
	return false
}
func (c Config) subjectPoolHost(host string) bool {
	for _, p := range c.SubjectPools {
		u, err := url.Parse(p.Endpoint)
		if err == nil && u.Hostname() == host {
			return true
		}
	}
	return false
}
func (s *Snapshot) validateSubjectPools() error {
	if len(s.SubjectPools) > 0 && strings.TrimSpace(s.SubjectPoolAPIKey) == "" {
		return errors.New("MODEL_GATEWAY_SUBJECT_POOL_API_KEY is required for subject pools")
	}
	for model, p := range s.SubjectPools {
		if model != protocol.SegmentSubjectModel || p.Protocol != protocol.SegmentSubjectProtocol || p.Endpoint == "" || strings.HasSuffix(p.Endpoint, "/") || p.HealthURL != p.Endpoint+"/ready" {
			return errors.New("invalid subject pool model, protocol or health URL")
		}
		if err := s.ValidateEndpoint(p.Endpoint, false); err != nil {
			return errors.New("invalid subject pool endpoint")
		}
		if err := s.ValidateEndpoint(p.HealthURL, true); err != nil {
			return errors.New("invalid subject pool health URL")
		}
	}
	return nil
}
func (s *Snapshot) privateBackendIP(ip netip.Addr) bool {
	if !ip.IsPrivate() {
		return false
	}
	for _, network := range s.networks {
		if network.Contains(net.IP(ip.AsSlice())) {
			return true
		}
	}
	return false
}

type poolPolicyKey struct{}

func WithSubjectPoolPolicy(ctx context.Context, s *Snapshot) context.Context {
	return context.WithValue(ctx, poolPolicyKey{}, s)
}

// SubjectPoolDialer pins all DNS answers to the configured private allowlist.
// Callers preserve their default dial path for existing fixed-IP protocols.
func SubjectPoolDialer(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		s, ok := ctx.Value(poolPolicyKey{}).(*Snapshot)
		if !ok {
			return dial(ctx, network, address)
		}
		targets, err := s.ResolveBackend(ctx, address, net.DefaultResolver.LookupNetIP)
		if err != nil {
			return nil, err
		}
		var failures []error
		for _, target := range targets {
			conn, err := dial(ctx, network, target)
			if err == nil {
				return conn, nil
			}
			failures = append(failures, err)
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.Join(failures...)
	}
}
