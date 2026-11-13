package store

import (
	"encoding/json"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"testing"
)

func TestMetadataDoesNotChangeIdempotentInput(t *testing.T) {
	input := json.RawMessage(`{"prompt":"same"}`)
	for _, metadata := range []protocol.Metadata{{}, {DeviceID: "device", UserID: "user", TraceID: "trace", TraceParent: "parent"}} {
		payload, _ := json.Marshal(protocol.Submission{Input: input, Metadata: metadata})
		if !sameInput(payload, input) {
			t.Fatal("metadata changed task identity")
		}
		if sameInput(payload, json.RawMessage(`{"prompt":"different"}`)) {
			t.Fatal("business change accepted")
		}
	}
}
