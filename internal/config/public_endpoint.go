package config

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

var dnsName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$`)

func validPublicHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return publicIP(ip)
	}
	return len(host) <= 253 && dnsName.MatchString(strings.TrimPrefix(host, "*."))
}

func (c Config) publicHostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, allowed := range c.PublicHosts {
		if host == allowed || (strings.HasPrefix(allowed, "*.") &&
			strings.HasSuffix(host, allowed[1:]) && len(host) > len(allowed)-1) {
			return true
		}
	}
	return false
}

func publicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	// Shared address space and special-purpose ranges are not public model hosts.
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48", "::/96", "2002::/16", "100::/64"} {
		if netip.MustParsePrefix(cidr).Contains(address) {
			return false
		}
	}
	return true
}

// ResolveBackend resolves once, checks every answer, and returns numeric dial
// addresses. The caller must dial these addresses rather than resolve again.
// TLS still verifies the original URL hostname through http.Transport.
func (s *Snapshot) ResolveBackend(ctx context.Context, address string,
	lookup func(context.Context, string, string) ([]netip.Addr, error)) ([]string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !s.ports[port] {
		return nil, errors.New("invalid backend dial address")
	}
	if s.publicHostAllowed(host) {
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			ips, err = lookup(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
		}
		if len(ips) == 0 {
			return nil, errors.New("backend DNS returned no addresses")
		}
		out := make([]string, 0, len(ips))
		for _, ip := range ips {
			if !publicIP(net.IP(ip.AsSlice())) {
				return nil, errors.New("public backend resolved to a non-public address")
			}
			out = append(out, net.JoinHostPort(ip.String(), port))
		}
		return out, nil
	}
	ip := net.ParseIP(host)
	for _, network := range s.networks {
		if ip != nil && network.Contains(ip) {
			return []string{net.JoinHostPort(host, port)}, nil
		}
	}
	return nil, errors.New("backend dial address is outside the allowlist")
}
