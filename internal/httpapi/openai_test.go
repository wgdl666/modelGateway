package httpapi

import (
	"bytes"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/wgdl666/modelGateway/internal/config"
)

func TestGenerationRoutesRejectBeforeAdmission(t *testing.T) {
	cfg := config.Config{APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24),
		AllowedNetworks: []string{"172.31.0.0/16"}, AllowedPorts: []string{"8001"}}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := New(nil, runtime, nil)
	engine := server.HTTPServer(listener).Engine
	for _, tc := range []struct {
		path, body, token string
		code              int
	}{
		{openAIGenerationsPath, `{"prompt":"x"}`, cfg.APIToken, 400},
		{openAIGenerationsPath, `{"model":"arbitrary-model","prompt":"x","expert":"body_model"}`, cfg.APIToken, 400},
		{openAIGenerationsPath, `{"model":"arbitrary-model","prompt":"x","garment_name":"coat"}`, cfg.APIToken, 400},
		{openAIGenerationsPath, `{"model":"arbitrary-model","prompt":"x","seconds":5}`, cfg.APIToken, 400},
		{openAIGenerationsPath, `{"model":"arbitrary-model","prompt":"x","n":0}`, cfg.APIToken, 400},
		{openAIVideosPath, `{"model":"any-video","prompt":"x","seconds":-1}`, cfg.APIToken, 400},
		{openAIVideosPath, `{"model":"any-video","prompt":"x","n":2}`, cfg.APIToken, 400},
		{openAIVideosPath, `{"model":"any-video","prompt":"x","parameters":null}`, cfg.APIToken, 400},
		{openAIVideosPath, `{"model":"any-video","prompt":"x"}`, cfg.AdminToken, 401},
		{"/v1/gateway/tasks", `{}`, cfg.APIToken, 404},
	} {
		res := ut.PerformRequest(engine, http.MethodPost, tc.path, &ut.Body{Body: strings.NewReader(tc.body), Len: len(tc.body)},
			ut.Header{Key: "Authorization", Value: "Bearer " + tc.token}, ut.Header{Key: "Content-Type", Value: "application/json"})
		if res.Code != tc.code {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, res.Code, res.Body.String())
		}
		if len(server.uploads) != 0 {
			t.Fatal("rejected request leaked upload slot")
		}
	}
	res := ut.PerformRequest(engine, http.MethodPost, openAIGenerationsPath, nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + cfg.APIToken}, ut.Header{Key: "Idempotency-Key", Value: "bad"})
	if res.Code != 400 || !strings.Contains(res.Body.String(), "Idempotency-Key") {
		t.Fatalf("invalid key: %d %s", res.Code, res.Body.String())
	}
}

func TestGenerationMultipartPreservesImagesAndTypedParameters(t *testing.T) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for key, value := range map[string]string{"model": "new-image-model", "prompt": "combine", "n": "3", "size": "2048x1024", "parameters": `{"seed":9007199254740993,"enable":false}`} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"image[]", "image"} {
		part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="` + name + `"; filename="ref.png"`}, "Content-Type": {"image/png"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	c := app.NewContext(0)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	c.Request.SetBody(form.Bytes())
	req, err := readOpenAIForm(c, 1<<20, "image", []string{"model", "prompt", "n", "size", "parameters", "image", "image[]"})
	if err != nil || req.Model != "new-image-model" || req.N == nil || *req.N != 3 || len(req.Images) != 2 ||
		string(req.Images[0].Data) != "image[]" || string(req.Images[1].Data) != "image" || string(req.Parameters) != `{"seed":9007199254740993,"enable":false}` {
		t.Fatalf("multipart lost data: %+v %v", req, err)
	}
}
