package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestGenerationKeepsUnknownAndZeroDistinct(t *testing.T) {
	var result Result
	raw := []byte(`{"generation":{"effective_prompt":"最终 prompt","pe_status":"disabled","parameters":{"seed":0,"guidance":null,"width":768,"height":1024,"steps":8,"output_format":"png"},"timings":{"pe_ms":0,"total_ms":null},"retries":{"generation":{"count":0}},"raw_response":"not public"},"data":[{"b64_json":"not public"}]}`)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	g := result.Generation
	if g.Parameters.Seed == nil || *g.Parameters.Seed != 0 || g.Parameters.Guidance != nil || g.Timings.PEMS == nil || *g.Timings.PEMS != 0 || g.Timings.TotalMS != nil || g.Retries.Generation.Count != 0 {
		t.Fatalf("zero/null receipt changed: %+v", g)
	}
	saved, err := json.Marshal(result)
	if err != nil || bytes.Contains(saved, []byte("not public")) {
		t.Fatalf("unrelated media/debug data leaked: %s %v", saved, err)
	}
	var historical Result
	if err := json.Unmarshal([]byte(`{"files":[]}`), &historical); err != nil || historical.Generation != nil {
		t.Fatal("historical result fabricated generation metadata")
	}
}
