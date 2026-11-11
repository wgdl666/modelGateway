package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestMattingSelectsRegisteredProtocolWithinSubmission(t *testing.T) {
	ledger, _, db := stores(t)
	cfg := testConfig()
	cfg.VisionAddr = "172.31.43.7:18280"
	var expectedPath, expectedMethod, expectedPreprocess string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != expectedPath || r.FormValue("method") != expectedMethod || r.FormValue("preprocess") != expectedPreprocess ||
			r.FormValue("image") != "aW1hZ2U=" || r.FormValue("output") != "b64" || r.FormValue("person_method") != "" || r.FormValue("subject_method") != "" {
			t.Errorf("wrong native matting request: %s %+v", r.URL.Path, r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"refined_png_b64":"cG5n"}`)
	}))
	defer upstream.Close()
	base := synchronousServer(t, ledger, cfg, upstream)
	for _, tc := range []struct{ model, kind, path, method, preprocess string }{
		{"new-person-key", protocol.SegmentPersonProtocol, "/segment", "person-algorithm", "false"},
		{"new-subject-key", protocol.SegmentSubjectProtocol, "/segment_subject", "subject-algorithm", ""},
	} {
		t.Run(tc.model, func(t *testing.T) {
			spec := testInstance(protocol.SegmentPersonModel, "http://127.0.0.1:6106", 1)
			spec.ModelKey, spec.Protocol = tc.model, tc.kind
			spec.HealthURL = cfg.SynchronousHealthURL(spec.Protocol)
			registerReady(t, ledger, spec)
			expectedPath, expectedMethod, expectedPreprocess = "/_dispatch/6106/segment-person/api"+tc.path, tc.method, tc.preprocess
			id := uuid.NewString()
			resp := sendSynchronous(t, base, "/v1/vision/segment-person/api/matte", "application/json",
				`{"image":"aW1hZ2U=","output":"b64","method":"fallback","person_method":"person-algorithm","subject_method":"subject-algorithm"}`, id, tc.model)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(raw) != `{"refined_png_b64":"cG5n"}` {
				t.Fatalf("matting result: %d %s", resp.StatusCode, raw)
			}
			row := waitSynchronousState(t, db, id, request.StateSucceeded, false)
			if row.Protocol != tc.kind || row.ModelKey != tc.model {
				t.Fatalf("wrong admitted protocol: %+v", row)
			}
		})
	}
}

func TestDecisionUsesTextSubmissionWithoutProtocolLookup(t *testing.T) {
	ledger, _, db := stores(t)
	cfg := testConfig()
	cfg.DecisionAddr = "172.31.3.219:8874"
	const prediction = `{"prediction":{"label":"shirt","probability":0.8}}`
	response := prediction
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		image, header, err := r.FormFile("image")
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		raw, _ := io.ReadAll(image)
		image.Close()
		if r.URL.Path != "/v1/instances/19101/predict" || header.Filename != "image.png" || string(raw) != "\x89PNG\r\n\x1a\n" || r.FormValue("task") != "image_type" {
			t.Errorf("Decision input changed: %s %+v", r.URL, r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, response)
	}))
	defer upstream.Close()
	base := synchronousServer(t, ledger, cfg, upstream)
	spec := testInstance(protocol.DecisionModel, "http://127.0.0.1:19101", 1)
	spec.ModelKey = "registered-decision-v2"
	spec.HealthURL = cfg.SynchronousHealthURL(spec.Protocol)
	registerReady(t, ledger, spec)
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{
			"model": spec.ModelKey, "stream": stream, "model_traits": map[string]string{"task": "image_type"},
			"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64,iVBORw0KGgo="}}}}},
		})
		id := uuid.NewString()
		resp := sendSynchronous(t, base, "/v1/chat/completions", "application/json", string(body), id)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("Decision text result: %d %s", resp.StatusCode, raw)
		}
		if stream {
			if !strings.HasPrefix(string(raw), "data: ") || !strings.HasSuffix(string(raw), "data: [DONE]\n\n") {
				t.Fatalf("invalid Decision stream: %s", raw)
			}
			raw = []byte(strings.Split(strings.TrimPrefix(string(raw), "data: "), "\n\n")[0])
		}
		var result map[string]any
		if json.Unmarshal(raw, &result) != nil || result["model"] != spec.ModelKey {
			t.Fatalf("invalid Decision chat response: %s", raw)
		}
		choice := result["choices"].([]any)[0].(map[string]any)
		key := "message"
		if stream {
			key = "delta"
		}
		if choice[key].(map[string]any)["content"] != prediction {
			t.Fatalf("native prediction changed: %s", raw)
		}
		waitSynchronousState(t, db, id, request.StateSucceeded, false)
	}
	// HTTP 200 的坏分类也必须收敛为失败并释放占用，不能只在 ModelHub 返回错误。
	response = `{"prediction":{"label":"shirt"}}`
	id := uuid.NewString()
	resp := sendSynchronous(t, base, "/v1/chat/completions", "application/json",
		`{"model":"registered-decision-v2","model_traits":{"task":"image_type"},"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`, id)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("invalid prediction returned %d", resp.StatusCode)
	}
	waitSynchronousState(t, db, id, request.StateFailed, false)
}
