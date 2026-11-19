package httpapi

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"reflect"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/internal/inputschema"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestDecodeSynchronousSubmissionPreservesInput(t *testing.T) {
	const original = `{ "image": "aW1hZ2U=", "number": 9007199254740993 }`
	raw := []byte(`{"input":{"replaced":true},"IN\u0050UT" : ` + original + `,
		"metadata":{"trace_id":"test-trace"},"Metadata":{"device_id":"device"}}`)
	var previous protocol.Submission
	if err := protocol.DecodeJSON(raw, &previous); err != nil {
		t.Fatal(err)
	}
	got, value, err := decodeSynchronousSubmission(raw)
	if err != nil || !reflect.DeepEqual(got, previous) {
		t.Fatalf("decoded submission = %+v, %v, want %+v", got, err, previous)
	}
	offset := bytes.Index(raw, []byte(original))
	if &got.Input[0] != &raw[offset] {
		t.Fatal("input should reference the request body")
	}
	fields, err := inputschema.ValidateValue(json.RawMessage(`{
		"type":"object","required":["number"],"properties":{"number":{"const":9007199254740993}},
		"additionalProperties":true
	}`), value, false)
	if err != nil || fields["number"] != json.Number("9007199254740993") || fields["replaced"] != nil {
		t.Fatalf("validated input = %v, %v", fields, err)
	}
}

func TestDecodeSynchronousSubmissionRejectsMalformedEnvelope(t *testing.T) {
	for _, raw := range []string{
		`{"input":{}} []`, `{"input":{},}`, `{"input":`, `{"input" {}}`, `[]`,
		`{"input":{},"extra":true}`, `{"input":{},"output":1}`, `{"input":{},"metadata":{"extra":true}}`,
	} {
		if _, _, err := decodeSynchronousSubmission([]byte(raw)); err == nil {
			t.Errorf("expected invalid envelope %s", raw)
		}
	}
}

func TestUnifiedVisionEncoding(t *testing.T) {
	for _, tc := range []struct{ kind, method string }{{protocol.SegmentPersonProtocol, "person-algorithm"}, {protocol.SegmentSubjectProtocol, "subject-algorithm"}} {
		for _, route := range visionRoutes {
			if route.protocol != tc.kind {
				continue
			}
			raw := json.RawMessage(`{"file":"aW1hZ2U=","output":"b64","method":"fallback","person_method":"person-algorithm","subject_method":"subject-algorithm"}`)
			schema, err := inputschema.Seed(tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			fields, err := inputschema.Validate(schema, raw, false)
			if err != nil {
				t.Fatal(err)
			}
			body, contentType, err := unifiedVisionBody(tc.kind, route, raw, fields)
			if err != nil {
				t.Fatal(err)
			}
			_, params, _ := mime.ParseMediaType(contentType)
			form, err := multipart.NewReader(strings.NewReader(string(body)), params["boundary"]).ReadForm(1024)
			if err != nil {
				t.Fatal(err)
			}
			defer form.RemoveAll()
			if form.Value["method"][0] != tc.method || len(form.File["file"]) != 1 || form.Value["person_method"] != nil || form.Value["subject_method"] != nil {
				t.Fatalf("wrong conversion: %+v", form)
			}
			if tc.kind == protocol.SegmentPersonProtocol && form.Value["preprocess"][0] != "false" {
				t.Fatal("person preprocessing changed")
			}
		}
	}
}
