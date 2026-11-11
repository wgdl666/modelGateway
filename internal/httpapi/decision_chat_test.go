package httpapi

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

func TestDecisionChatPreservesPresetAndInlineImage(t *testing.T) {
	fields := map[string]json.RawMessage{
		"messages":     json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]`),
		"model_traits": json.RawMessage(`{"task":"image_type"}`), "stream": json.RawMessage(`true`),
	}
	body, contentType, stream, err := prepareDecisionChat(fields)
	if err != nil || !stream {
		t.Fatalf("prepare Decision: %v", err)
	}
	_, params, _ := mime.ParseMediaType(contentType)
	reader := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	form, err := reader.ReadForm(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer form.RemoveAll()
	if form.Value["task"][0] != "image_type" || len(form.Value["prompt"]) != 0 || len(form.File["image"]) != 1 || form.File["image"][0].Filename != "image.png" {
		t.Fatalf("native form changed: %+v", form)
	}
	file, _ := form.File["image"][0].Open()
	raw, _ := io.ReadAll(file)
	file.Close()
	if string(raw) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("image bytes changed")
	}
	for _, tc := range []struct{ name, messages, traits string }{
		{"remote image", `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://private/image.png"}}]}]`, `{"task":"image_type"}`},
		{"missing image", `[{"role":"user","content":"classify"}]`, `{}`},
		{"invalid image", `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,dGV4dA=="}}]}]`, `{"task":"image_type"}`},
		{"mixed task and prompt", `[{"role":"user","content":[{"type":"text","text":"classify"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]`, `{"task":"image_type"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields["messages"], fields["model_traits"] = json.RawMessage(tc.messages), json.RawMessage(tc.traits)
			if _, _, _, err := prepareDecisionChat(fields); err == nil {
				t.Fatal("invalid Decision input was accepted")
			}
		})
	}
}
