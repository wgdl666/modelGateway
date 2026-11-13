package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestGenerationDispatchUsesRegisteredModelAndSeparateContracts(t *testing.T) {
	for _, tc := range []struct{ kind, path, input string }{
		{protocol.ImageGenerationProtocol, "/v1/images/generations", `{"prompt":"draw","n":3,"size":"1600x900","parameters":{"seed":9007199254740993,"switch":false,"model":"not-a-router","nested":{"weights":[0.2,0.8]}}}`},
		{protocol.VideoGenerationProtocol, "/v1/videos", `{"prompt":"animate","seconds":3.5,"parameters":{"fps":24,"audio":true}}`},
	} {
		for _, model := range []string{"unseen-model-A", "unseen-model-B"} {
			path, raw, err := generationRequest(&ent.Request{ModelKey: model, Protocol: tc.kind}, protocol.Submission{
				Input: json.RawMessage(tc.input), Metadata: protocol.Metadata{UserID: "caller"}})
			if err != nil || path != tc.path {
				t.Fatalf("dispatch: %s %s %v", path, raw, err)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || string(fields["model"]) != `"`+model+`"` || string(fields["user"]) != `"caller"` {
				t.Fatalf("model identity lost: %s", raw)
			}
			var original map[string]json.RawMessage
			_ = json.Unmarshal([]byte(tc.input), &original)
			for key, value := range original {
				if string(fields[key]) != string(value) {
					t.Errorf("%s changed: %s -> %s", key, value, fields[key])
				}
			}
			if strings.Contains(string(raw), "garment_name") || strings.Contains(string(raw), "expert") {
				t.Fatalf("business routing leaked: %s", raw)
			}
		}
	}
}

func TestGenerationResponseMustIdentifyRequestedModel(t *testing.T) {
	for _, raw := range []string{`{"data":[]}`, `{"model":"different","data":[]}`, `{"model":null}`, `invalid`} {
		if generationModelMatches([]byte(raw), "selected") {
			t.Fatalf("accepted missing/wrong model: %s", raw)
		}
	}
	if !generationModelMatches([]byte(`{"model":"selected","data":[]}`), "selected") {
		t.Fatal("rejected matching model")
	}
}
