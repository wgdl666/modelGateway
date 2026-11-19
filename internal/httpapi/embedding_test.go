package httpapi

import (
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"testing"
)

func TestEmbeddingUsesClaimedReplica(t *testing.T) {
	endpoint, pool := synchronousEndpoint(config.Config{}, &ent.Request{Protocol: protocol.EmbeddingProtocol, Endpoint: "http://172.31.3.219:8823"}, "/v1/embed")
	if pool || endpoint != "http://172.31.3.219:8823/v1/embed" {
		t.Fatalf("unexpected destination %s pool=%v", endpoint, pool)
	}
	if !protocol.ValidProtocol(protocol.EmbeddingProtocol) || protocol.MediaProtocol(protocol.EmbeddingProtocol) {
		t.Fatal("embedding must use synchronous execution")
	}
}
