package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"

	"github.com/wgdl666/modelGateway/internal/config"
)

func TestBackendDialerDoesNotResolveHostnameAgain(t *testing.T) {
	runtime, err := config.NewRuntime(config.Config{DatabaseURL: "test", APIToken: "test-api-token-at-least-24-chars",
		AdminToken: "test-admin-token-at-least-24-chars", PublicHosts: []string{"node.seetacloud.com"}, AllowedPorts: []string{"8443"}})
	if err != nil {
		t.Fatal(err)
	}
	lookups, dials := 0, 0
	lookup := func(_ context.Context, _, _ string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dial := func(_ context.Context, network, target string) (net.Conn, error) {
		dials++
		if target != "8.8.8.8:8443" || network != "tcp" {
			t.Fatalf("unpinned dial: %s %s", network, target)
		}
		return nil, errors.New("test dial failure")
	}
	ctx := context.WithValue(context.Background(), backendPolicyKey{}, runtime.Current())
	_, _ = backendDialer(lookup, dial)(ctx, "tcp", "node.seetacloud.com:8443")
	if lookups != 1 || dials != 1 {
		t.Fatalf("lookups=%d dials=%d", lookups, dials)
	}
	bad := func(_ context.Context, _, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	_, err = backendDialer(bad, dial)(ctx, "tcp", "node.seetacloud.com:8443")
	if err == nil || dials != 1 {
		t.Fatal("forbidden DNS answer reached dialer")
	}
}

// Numeric dialing must preserve TLS hostname verification for public endpoints.
func TestPublicEndpointTLSVerifiesRegisteredHostname(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "example.com" {
			t.Errorf("lost TLS hostname: %s", r.TLS.ServerName)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	// httptest's certificate contains example.com, but not wrong.invalid.
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	parsed, _ := url.Parse(server.URL)
	runtime, err := config.NewRuntime(config.Config{DatabaseURL: "test",
		APIToken: "test-api-token-at-least-24-chars", AdminToken: "test-admin-token-at-least-24-chars",
		PublicHosts: []string{"example.com", "wrong.invalid"}, AllowedPorts: []string{parsed.Port()}})
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	transport.DialContext = backendDialer(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort("8.8.8.8", parsed.Port()) {
			t.Errorf("unpinned address: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, parsed.Host)
	})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, host := range []string{"example.com", "wrong.invalid"} {
		endpoint := "https://" + net.JoinHostPort(host, parsed.Port()) + "/gpu1/health"
		if err := runtime.Current().ValidateEndpoint(endpoint, true); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(t.Context(), backendPolicyKey{}, runtime.Current())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		response, err := client.Do(req)
		if host == "wrong.invalid" {
			if err == nil {
				response.Body.Close()
				t.Fatal("accepted certificate for another hostname")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
}
