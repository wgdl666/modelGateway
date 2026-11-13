package inputschema

import (
	"encoding/json"
	"testing"
)

func TestSeedsAndCredentialFields(t *testing.T) {
	entries, err := seeds.ReadDir("seeds")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := seeds.ReadFile("seeds/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckDefinition(raw); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
		}
	}
	schema, _ := Seed("vton")
	for _, tc := range []struct {
		input               string
		experimental, valid bool
	}{
		{`{"images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/png","data":"eA=="}],"prompt":"try on"}`, false, true},
		{`{"images":[{"mime_type":"image/png","data":"eA=="}]}`, false, false},
		{`{"images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/png","data":"eA=="}],"use_pe":"false"}`, false, false},
		{`{"images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/png","data":"eA=="}],"use_pe":"false"}`, true, true},
	} {
		if err := Validate(schema, json.RawMessage(tc.input), tc.experimental); (err == nil) != tc.valid {
			t.Errorf("valid=%v: %v", tc.valid, err)
		}
	}
}

func TestSchemaOwnsValidationAndRejectsRemoteRefs(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["thinking_level"],"properties":{"thinking_level":{"enum":["high"]}},"additionalProperties":false}`)
	if err := Validate(schema, json.RawMessage(`{"thinking_level":"high"}`), false); err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, json.RawMessage(`{"thinking_level":"low"}`), false); err == nil {
		t.Fatal("enum was not enforced")
	}
	if err := CheckDefinition(json.RawMessage(`{"$ref":"https://example.com/remote.json"}`)); err == nil {
		t.Fatal("remote reference accepted")
	}
	chat, _ := Seed("chat_completions")
	if err := Validate(chat, json.RawMessage(`{"messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"tool","content":"ok"}],"reasoning_effort":"high","temperature":0}`), false); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaPreservesIntegerEnumPrecision(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"seed":{"enum":[9007199254740993]}},"required":["seed"]}`)
	if err := Validate(raw, json.RawMessage(`{"seed":9007199254740993}`), false); err != nil {
		t.Fatal(err)
	}
	if err := Validate(raw, json.RawMessage(`{"seed":9007199254740992}`), false); err == nil {
		t.Fatal("schema integer enum rounded during credential expansion")
	}
}
