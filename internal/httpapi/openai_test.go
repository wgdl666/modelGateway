package httpapi

import (
	"bytes"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/wgdl666/modelGateway/internal/config"
)

func TestOpenAIRoutesRejectBeforeAdmission(t *testing.T) {
	cfg := config.Config{
		APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24),
		AllowedNetworks: []string{"172.31.0.0/16"}, AllowedPorts: []string{"8001"},
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	engine := New(nil, runtime, nil).HTTPServer(listener).Engine
	call := func(method, path, contentType, body string, headers ...ut.Header) *ut.ResponseRecorder {
		items := []ut.Header{{Key: "Authorization", Value: "Bearer " + cfg.APIToken}, {Key: "Content-Type", Value: contentType}}
		return ut.PerformRequest(engine, method, path, &ut.Body{Body: strings.NewReader(body), Len: len(body)}, append(items, headers...)...)
	}
	generation := call(http.MethodPost, openAIGenerationsPath, "application/json", `{"model":"virtual_tryon","prompt":"a coat"}`)
	if generation.Code != 400 || !strings.Contains(generation.Body.String(), "image generation has no expert") {
		t.Fatalf("generation status=%d body=%s", generation.Code, generation.Body.String())
	}
	badKey := call(http.MethodPost, openAIGenerationsPath, "application/json", `{"model":"virtual_tryon","prompt":"a coat"}`, ut.Header{Key: "Idempotency-Key", Value: "not-a-uuid"})
	if badKey.Code != 400 || !strings.Contains(badKey.Body.String(), "Idempotency-Key must be a UUID") {
		t.Fatalf("idempotency status=%d body=%s", badKey.Code, badKey.Body.String())
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("model", "virtual_tryon"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "try on"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="image"; filename="person.png"`},
		"Content-Type":        {"image/png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("png")); err != nil || writer.Close() != nil {
		t.Fatal(err)
	}
	edit := call(http.MethodPost, openAIEditsPath, writer.FormDataContentType(), form.String())
	if edit.Code != 400 || !strings.Contains(edit.Body.String(), "virtual_tryon requires two images") {
		t.Fatalf("edit status=%d body=%s", edit.Code, edit.Body.String())
	}
}
