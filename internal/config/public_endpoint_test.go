package config

import (
	"context"
	"net/netip"
	"reflect"
	"testing"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

func endpointConfig() Config {
	return Config{DatabaseURL: "test", APIToken: "test-api-token-at-least-24-chars",
		AdminToken: "test-admin-token-at-least-24-chars", AllowedNetworks: []string{"10.0.0.0/8"},
		AllowedPorts: []string{"8001", "8443", "443"}, PublicHosts: []string{"*.seetacloud.com", "8.8.8.8"},
		NativeModels: map[string]NativeBackend{protocol.OutfitModel: {}, protocol.BodyModel: {}, protocol.VideoModel: {Video: true}}}
}

func TestRegisteredEndpointsSelectProtocolWithoutAddressBindings(t *testing.T) {
	runtime, err := NewRuntime(endpointConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.Current()
	for _, endpoint := range []string{"http://10.2.3.4:8001", "https://node.westd.seetacloud.com:8443/vworld/gpu3", "https://8.8.8.8/model/gpu9"} {
		for _, model := range []string{protocol.OutfitModel, protocol.BodyModel, protocol.VideoModel} {
			health := endpoint + "/health"
			if model == protocol.VideoModel {
				health = endpoint + "/healthz"
			}
			kind := map[string]string{protocol.OutfitModel: protocol.OutfitProtocol, protocol.BodyModel: protocol.BodyProtocol, protocol.VideoModel: protocol.VideoProtocol}[model]
			target := protocol.InstanceTarget{ModelKey: model, Protocol: kind, Endpoint: endpoint, HealthURL: health}
			if err := s.ValidateInstanceTarget(target); err != nil {
				t.Fatalf("%+v: %v", target, err)
			}
			if backend, ok := s.NativeBackendFor(model, endpoint); !ok || !backend.Matches(kind) {
				t.Fatal("protocol not selected by model")
			}
		}
	}
	if len(s.NativeBackends) != 0 {
		t.Fatal("test must not preconfigure any GPU endpoint")
	}
	// An instance can be added at a new allowed address without reloading config.
	if err := s.ValidateInstanceTarget(protocol.InstanceTarget{ModelKey: protocol.OutfitModel, Protocol: protocol.OutfitProtocol,
		Endpoint: "https://new-host.bjb2.seetacloud.com:8443/gpu7", HealthURL: "https://new-host.bjb2.seetacloud.com:8443/gpu7/health"}); err != nil {
		t.Fatal(err)
	}
	if s.AcceptsInstanceAddress(protocol.OutfitModel, protocol.OutfitProtocol, "http://10.0.0.1:8001", "http://10.0.0.2:8001/health") {
		t.Fatal("cross-origin image probe accepted")
	}
	if s.AcceptsInstanceAddress(protocol.VideoModel, protocol.VideoProtocol, "http://10.0.0.1:8001/gpu1", "http://10.0.0.1:8001/gpu0/healthz") {
		t.Fatal("cross-GPU video probe accepted")
	}
	cfg := endpointConfig()
	cfg.NativeModels = nil
	if err := runtime.Reload(cfg); err == nil {
		t.Fatal("protocol changes must drain and restart")
	}
}

func TestPublicEndpointValidation(t *testing.T) {
	runtime, err := NewRuntime(endpointConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"http://node.westd.seetacloud.com:8443/gpu0", "https://seetacloud.com:8443/gpu0",
		"https://node.seetacloud.com.evil.test:8443/gpu0", "https://node.seetacloud.com:9999/gpu0",
		"https://user:password@node.seetacloud.com:8443/gpu0", "https://node.seetacloud.com:8443/gpu0?target=1",
		"https://node.seetacloud.com:8443/gpu0#fragment", "https://node.seetacloud.com:8443/vworld/../gpu1",
		"https://node.seetacloud.com:8443/vworld/%67pu1", "https://node.seetacloud.com:8443//gpu0",
		"http://169.254.169.254:8001", "http://127.0.0.1:8001", "https://unapproved.example:8443/gpu0",
	} {
		if err := runtime.Current().ValidateEndpoint(endpoint, false); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	for _, pattern := range []string{"*", "*.com", "*seetacloud.com", "https://node.seetacloud.com", "node.seetacloud.com/gpu0", "127.0.0.1"} {
		cfg := endpointConfig()
		cfg.PublicHosts = []string{pattern}
		if _, err := NewRuntime(cfg); err == nil {
			t.Errorf("accepted host rule %q", pattern)
		}
	}
}

func TestPublicDNSIsCheckedOnEveryDialAndPinned(t *testing.T) {
	runtime, err := NewRuntime(endpointConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.Current()
	answers := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2606:4700:4700::1111")}
	calls := 0
	lookup := func(_ context.Context, network, host string) ([]netip.Addr, error) {
		calls++
		if network != "ip" || host != "node.seetacloud.com" {
			t.Fatal("unexpected lookup")
		}
		return answers, nil
	}
	got, err := s.ResolveBackend(context.Background(), "node.seetacloud.com:8443", lookup)
	want := []string{"8.8.8.8:8443", "[2606:4700:4700::1111]:8443"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses %v: %v", got, err)
	}
	for _, blocked := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.100.100.200", "::1", "fc00::1", "::ffff:10.1.2.3"} {
		answers = []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(blocked)}
		if _, err := s.ResolveBackend(context.Background(), "node.seetacloud.com:8443", lookup); err == nil {
			t.Errorf("mixed DNS accepted %s", blocked)
		}
	}
	if calls != 8 {
		t.Fatalf("DNS result reused across dials: %d", calls)
	}
	if _, err := s.ResolveBackend(context.Background(), "evil.example:8443", lookup); err == nil {
		t.Fatal("unapproved host dialed")
	}
	got, err = s.ResolveBackend(context.Background(), "10.1.2.3:8001", lookup)
	if err != nil || len(got) != 1 || got[0] != "10.1.2.3:8001" {
		t.Fatal("approved private endpoint rejected")
	}
}
