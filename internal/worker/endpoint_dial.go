package worker

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"github.com/wgdl666/modelGateway/internal/config"
)

type backendPolicyKey struct{}

func backendDialer(lookup func(context.Context, string, string) ([]netip.Addr, error),
	dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		policy, ok := ctx.Value(backendPolicyKey{}).(*config.Snapshot)
		if !ok {
			// VLM/Decision/Vision probes use their own fixed private-IP configuration.
			return dial(ctx, network, address)
		}
		addresses, err := policy.ResolveBackend(ctx, address, lookup)
		if err != nil {
			return nil, err
		}
		var failures []error
		for _, target := range addresses {
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
