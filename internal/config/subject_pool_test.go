package config

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"testing"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

func poolConfig() Config {
	return Config{SubjectPoolAPIKey: "fixture-pool-key", APIToken: "test-client-token-at-least-24-chars", AdminToken: "test-admin-token-at-least-24-chars", AllowedNetworks: []string{"10.0.0.0/8"}, AllowedPorts: []string{"80"}, SubjectPools: map[string]SubjectPool{protocol.SegmentSubjectModel: {Protocol: protocol.SegmentSubjectProtocol, Endpoint: "http://pool.internal.test/internal/segment-subject-pool", HealthURL: "http://pool.internal.test/internal/segment-subject-pool/ready"}}}
}
func TestSubjectPoolExactRegistrationAndRestart(t *testing.T) {
	c := poolConfig()
	r, err := NewRuntime(c)
	if err != nil {
		t.Fatal(err)
	}
	p := c.SubjectPools[protocol.SegmentSubjectModel]
	target := protocol.InstanceTarget{ModelKey: protocol.SegmentSubjectModel, Protocol: p.Protocol, Endpoint: p.Endpoint, HealthURL: p.HealthURL}
	if err := r.Current().ValidateInstanceTarget(target); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"model", "protocol", "endpoint", "health"} {
		bad := target
		switch field {
		case "model":
			bad.ModelKey = protocol.SegmentPersonModel
		case "protocol":
			bad.Protocol = protocol.SegmentPersonProtocol
		case "endpoint":
			bad.Endpoint += "/other"
		case "health":
			bad.HealthURL += "/other"
		}
		if r.Current().ValidateInstanceTarget(bad) == nil {
			t.Errorf("accepted %s mismatch", field)
		}
	}
	next := poolConfig()
	delete(next.SubjectPools, protocol.SegmentSubjectModel)
	if r.Reload(next) == nil {
		t.Fatal("pool removal hot reload accepted")
	}
	next = poolConfig()
	p.Endpoint = "http://other.internal.test/internal/segment-subject-pool"
	p.HealthURL = p.Endpoint + "/ready"
	next.SubjectPools[protocol.SegmentSubjectModel] = p
	if r.Reload(next) == nil {
		t.Fatal("pool rebound hot reload accepted")
	}
	if err := r.Reload(poolConfig()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{func() Config {
		v := poolConfig()
		p := v.SubjectPools[protocol.SegmentSubjectModel]
		delete(v.SubjectPools, protocol.SegmentSubjectModel)
		v.SubjectPools[protocol.SegmentPersonModel] = p
		return v
	}(), func() Config {
		v := poolConfig()
		p := v.SubjectPools[protocol.SegmentSubjectModel]
		p.Protocol = protocol.SegmentPersonProtocol
		v.SubjectPools[protocol.SegmentSubjectModel] = p
		return v
	}()} {
		if _, err := NewRuntime(bad); err == nil {
			t.Fatal("person pool reuse accepted")
		}
	}
}
func TestSubjectPoolDNSChecksEveryPrivateAnswer(t *testing.T) {
	r, err := NewRuntime(poolConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ips []string
		ok  bool
	}{{[]string{"10.0.1.2", "10.0.2.3"}, true}, {[]string{"10.0.1.2", "8.8.8.8"}, false}, {[]string{"169.254.169.254"}, false}, {nil, false}, {[]string{"127.0.0.1"}, false}} {
		lookup := func(context.Context, string, string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, ip := range tc.ips {
				out = append(out, netip.MustParseAddr(ip))
			}
			return out, nil
		}
		_, err := r.Current().ResolveBackend(t.Context(), "pool.internal.test:80", lookup)
		if (err == nil) != tc.ok {
			t.Errorf("DNS %v: %v", tc.ips, err)
		}
		if _, err := r.Current().ResolveBackend(t.Context(), "unconfigured.internal.test:80", lookup); err == nil {
			t.Fatal("unconfigured DNS accepted")
		}
	}
}

func TestSubjectPoolRequiresSecret(t *testing.T) {
	c := poolConfig()
	c.SubjectPoolAPIKey = ""
	if _, err := NewRuntime(c); err == nil {
		t.Fatal("enabled pool without secret accepted")
	}
	c = poolConfig()
	r, _ := NewRuntime(c)
	c.SubjectPoolAPIKey = "changed"
	if r.Reload(c) == nil {
		t.Fatal("secret change accepted without restart")
	}
}

func TestSubjectPoolRejectsUnsafeConfiguredURLs(t *testing.T) {
	for _, endpoint := range []string{"http://8.8.8.8/pool", "http://169.254.169.254/pool", "http://127.0.0.1/pool", "http://10.0.0.2:8080/pool", "http://user:password@pool.internal.test/pool", "http://pool.internal.test/a/../pool", "http://pool.internal.test/%70ool", "http://pool.internal.test/pool?redirect=x", "http://pool.internal.test/pool#other", "file://pool.internal.test/pool"} {
		c := poolConfig()
		p := c.SubjectPools[protocol.SegmentSubjectModel]
		p.Endpoint = endpoint
		p.HealthURL = endpoint + "/ready"
		c.SubjectPools[protocol.SegmentSubjectModel] = p
		if _, err := NewRuntime(c); err == nil {
			t.Errorf("unsafe configured URL accepted: %s", endpoint)
		}
	}
}
func TestSubjectPoolSecretLoadsOnlyFromEnvironment(t *testing.T) {
	t.Setenv("MODEL_GATEWAY_SUBJECT_POOL_API_KEY", "env-fixture-secret")
	cfg, err := Parse([]byte(`{"database_url":"fixture","subject_pool_api_key":"untrusted-json-secret"}`))
	if err != nil || cfg.SubjectPoolAPIKey != "env-fixture-secret" {
		t.Fatalf("secret env load: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil || bytes.Contains(raw, []byte("env-fixture-secret")) {
		t.Fatal("secret serialized")
	}
	t.Setenv("MODEL_GATEWAY_SUBJECT_POOL_API_KEY", "")
	cfg, err = Parse([]byte(`{"database_url":"fixture","subject_pool_api_key":"untrusted-json-secret"}`))
	if err != nil || cfg.SubjectPoolAPIKey != "" {
		t.Fatal("JSON supplied pool secret")
	}
}
func TestSubjectPoolSnapshotClonesMapping(t *testing.T) {
	c := poolConfig()
	r, err := NewRuntime(c)
	if err != nil {
		t.Fatal(err)
	}
	delete(c.SubjectPools, protocol.SegmentSubjectModel)
	if len(r.Current().SubjectPools) != 1 {
		t.Fatal("caller mutation redirected snapshot")
	}
}
func TestSubjectPoolDialerPinsNumericAddressAndPreservesLegacy(t *testing.T) {
	c := poolConfig()
	p := c.SubjectPools[protocol.SegmentSubjectModel]
	p.Endpoint = "http://10.0.0.2/pool"
	p.HealthURL = p.Endpoint + "/ready"
	c.SubjectPools[protocol.SegmentSubjectModel] = p
	r, err := NewRuntime(c)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	dial := SubjectPoolDialer(func(_ context.Context, _ string, address string) (net.Conn, error) {
		targets = append(targets, address)
		a, b := net.Pipe()
		b.Close()
		return a, nil
	})
	ctx := WithSubjectPoolPolicy(t.Context(), r.Current())
	conn, err := dial(ctx, "tcp", "10.0.0.2:80")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := dial(ctx, "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("metadata address dialed")
	}
	conn, err = dial(t.Context(), "tcp", "legacy:6106")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if len(targets) != 2 || targets[0] != "10.0.0.2:80" || targets[1] != "legacy:6106" {
		t.Fatalf("unexpected dial targets: %v", targets)
	}
}
