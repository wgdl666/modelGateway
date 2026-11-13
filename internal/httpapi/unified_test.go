package httpapi

import (
	"encoding/json"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestUnifiedVisionEncoding(t *testing.T) {
	for _, tc := range []struct{ kind, method string }{{protocol.SegmentPersonProtocol, "person-algorithm"}, {protocol.SegmentSubjectProtocol, "subject-algorithm"}} {
		for _, route := range visionRoutes {
			if route.protocol != tc.kind {
				continue
			}
			raw := json.RawMessage(`{"file":"aW1hZ2U=","output":"b64","method":"fallback","person_method":"person-algorithm","subject_method":"subject-algorithm"}`)
			body, contentType, err := unifiedVisionBody(tc.kind, route, raw)
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
