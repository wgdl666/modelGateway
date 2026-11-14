package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerationInputsAreIndependentOfBusinessAndModel(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	for _, tc := range []struct{ kind, input string }{
		{ImageGenerationProtocol, `{"prompt":"a landscape","n":3,"size":"1536x1024","parameters":{"seed":0,"guidance":3.5,"enhance":false,"adapter":{"name":"any"}}}`},
		{ImageGenerationProtocol, `{"images":[` + image + `,` + image + `,` + image + `],"prompt":"combine references"}`},
		{ImageGenerationProtocol, `{"images":[` + image + `]}`},
		{VideoGenerationProtocol, `{"prompt":"a moving landscape","seconds":4.5,"size":"1280x720","parameters":{"fps":24,"audio":true}}`},
		{VideoGenerationProtocol, `{"images":[` + image + `,` + image + `],"prompt":"animate references"}`},
	} {
		if _, err := ValidateInput(tc.kind, json.RawMessage(tc.input)); err != nil {
			t.Errorf("%s: %v", tc.input, err)
		}
		normalized, err := NormalizeMediaInput(tc.kind, json.RawMessage(tc.input))
		if err != nil || string(normalized) != tc.input {
			t.Fatalf("generation input was rewritten: %s, %v", normalized, err)
		}
	}
}

func TestGenerationRejectsInvalidEnvelopeAndCrossMediaFields(t *testing.T) {
	for _, tc := range []struct{ kind, input string }{
		{ImageGenerationProtocol, `null`},
		{ImageGenerationProtocol, `{}`},
		{ImageGenerationProtocol, `{"prompt":"x","expert":"body_model"}`},
		{ImageGenerationProtocol, `{"prompt":"x","garment_name":"shirt"}`},
		{ImageGenerationProtocol, `{"prompt":"x","gender":"female"}`},
		{ImageGenerationProtocol, `{"prompt":"x","seconds":5}`},
		{ImageGenerationProtocol, `{"prompt":"x","n":0}`},
		{ImageGenerationProtocol, `{"prompt":"x","response_format":"url"}`},
		{ImageGenerationProtocol, `{"prompt":"x","parameters":[]}`},
		{ImageGenerationProtocol, `{"prompt":"x","parameters":null}`},
		{ImageGenerationProtocol, `{"images":[{"mime_type":"image/png","data":"!"}]}`},
		{ImageGenerationProtocol, `{"images":[{"mime_type":"text/plain","data":"eA=="}]}`},
		{VideoGenerationProtocol, `{"prompt":"x","n":2}`},
		{VideoGenerationProtocol, `{"prompt":"x","seconds":0}`},
		{VideoGenerationProtocol, `{"prompt":"x","parameters":"{}"}`},
	} {
		if err := ValidateGenerationInput(tc.kind, json.RawMessage(tc.input)); err == nil {
			t.Errorf("accepted %s: %s", tc.kind, tc.input)
		}
	}
}

func TestTextOnlyGenerationPreview(t *testing.T) {
	for _, kind := range []string{ImageGenerationProtocol, VideoGenerationProtocol} {
		params, refs, err := PreviewSubmission(kind, json.RawMessage(`{"input":{"prompt":"hello","parameters":{"seed":42}}}`))
		if err != nil || len(refs) != 0 || !strings.Contains(string(params), `"seed":42`) {
			t.Fatalf("text-only preview: %s %v %v", params, refs, err)
		}
	}
}
