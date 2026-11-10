package httpapi

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/wgdl666/modelGateway/internal/config"
)

func TestVisionInference(t *testing.T) {
	const (
		parser   = "/v1/vision/human-parser/human_parser/predict"
		segment  = "/v1/vision/segment-person/api/segment"
		image    = `{"image":"dGVzdA=="}`
		jsonType = "application/json"
	)
	cfg := config.Config{
		APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24),
		AllowedNetworks: []string{"10.0.0.0/8"}, AllowedPorts: []string{"8001"},
		MaxBodyBytes: 8 << 10,
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, runtime, nil)
	calls := 0
	s.direct.Transport = directRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		defer r.Body.Close()
		if r.Method != http.MethodPost || r.URL.Host != "172.31.43.7:18280" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected vision upstream: %s %s", r.Method, r.URL)
		}
		var kind, response string
		switch r.URL.Path {
		case "/human-parser/human_parser/predict":
			raw, err := io.ReadAll(r.Body)
			if err != nil || string(raw) != image || r.Header.Get("Content-Type") != jsonType {
				t.Fatal("parser must receive the inline JSON image")
			}
			kind, response = jsonType, `{"success":true}`
		case "/segment-person/api/segment":
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
			if err != nil || part.FormName() != "file" || part.FileName() == "" {
				t.Fatal("segmenter must receive a multipart upload")
			}
			raw, err := io.ReadAll(part)
			if err != nil || string(raw) != strings.Repeat("x", 4500) {
				t.Fatal("multipart upload was changed")
			}
			kind, response = "image/png", "png-data"
		default:
			t.Fatalf("unexpected vision path: %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}},
			Body: io.NopCloser(strings.NewReader(response)), ContentLength: int64(len(response)),
		}, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	h := s.HTTPServer(listener)
	call := func(path, token, kind, body string) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, http.MethodPost, path,
			&ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: kind}, ut.Header{Key: "Authorization", Value: "Bearer " + token})
	}
	if got := call(parser, cfg.APIToken, jsonType, image); got.Code != 404 {
		t.Fatalf("disabled vision route returned %d", got.Code)
	}
	cfg.VisionAddr = "172.31.43.7:18280"
	if err := runtime.Reload(cfg); err != nil {
		t.Fatal(err)
	}

	var form bytes.Buffer
	out := multipart.NewWriter(&form)
	if err := out.WriteField("s3_path", "bucket/private.png"); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, token, kind, body string
		status                        int
	}{
		{"admin token", parser, cfg.AdminToken, jsonType, image, 401},
		{"non inference path", "/v1/vision/human-parser/api/requests", cfg.APIToken, jsonType, image, 404},
		{"remote JSON", parser, cfg.APIToken, jsonType, `{"image_url":"http://internal/photo"}`, 400},
		{"remote multipart", segment, cfg.APIToken, out.FormDataContentType(), form.String(), 400},
		{"wrong content type", segment, cfg.APIToken, jsonType, image, 415},
		{"oversized image", parser, cfg.APIToken, jsonType, strings.Repeat("x", 8193), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := call(tc.path, tc.token, tc.kind, tc.body); got.Code != tc.status || calls != 0 {
				t.Fatalf("got %d and %d upstream calls, want %d and zero calls", got.Code, calls, tc.status)
			}
		})
	}
	if got := call(parser+"?image_url=http://internal/photo", cfg.APIToken, jsonType, image); got.Code != 200 || got.Body.String() != `{"success":true}` {
		t.Fatalf("parser response was not forwarded: %d", got.Code)
	}
	form.Reset()
	out = multipart.NewWriter(&form)
	file, err := out.CreateFormFile("file", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, strings.Repeat("x", 4500)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if got := call(segment, cfg.APIToken, out.FormDataContentType(), form.String()); got.Code != 200 ||
		string(got.Header().ContentType()) != "image/png" || got.Body.String() != "png-data" || calls != 2 {
		t.Fatalf("segment response was not forwarded: %d, calls %d", got.Code, calls)
	}
	cfg.VisionAddr = "8.8.8.8:18280"
	if runtime.Reload(cfg) == nil {
		t.Fatal("a public vision address must be rejected")
	}
}
