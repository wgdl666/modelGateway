package inputschema

import (
	"encoding/json"
	"testing"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestEmbeddingInput(t *testing.T) {
	schema, err := Seed(protocol.EmbeddingProtocol)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input string
		valid bool
	}{
		{`{"inputs":["蓝色衬衫"],"dim":1024,"normalize":true}`, true},
		{`{"inputs":[{"image":"data:image/png;base64,YQ==","text":"衬衫"}]}`, true},
		{`{"inputs":[]}`, false},
		{`{"inputs":[{}]}`, false},
		{`{"inputs":[{"image":"http://127.0.0.1/private"}]}`, false},
		{`{"inputs":[{"image":"/etc/passwd"}]}`, false},
		{`{"inputs":["x"],"dim":63}`, false},
		{`{"inputs":["x"],"normalize":"true"}`, false},
	} {
		_, err := Validate(schema, json.RawMessage(tc.input), false)
		if (err == nil) != tc.valid {
			t.Fatalf("input %s: %v", tc.input, err)
		}
	}
}
