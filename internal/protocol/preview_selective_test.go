package protocol

import (
	"encoding/json"
	"testing"
)

func TestPreviewOnlyDecodesSelectedImage(t *testing.T) {
	payload := json.RawMessage(`{"input":{"prompt":"keep","images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/jpeg","data":"not base64!"}]}}`)
	params, refs, err := PreviewSubmission(OutfitProtocol, payload)
	if err != nil || string(params) != `{"prompt":"keep"}` || len(refs) != 2 {
		t.Fatalf("detail decoded media: %s %v %v", params, refs, err)
	}
	img, err := PreviewInputImage(payload, 0)
	if err != nil || string(img.Data) != "x" {
		t.Fatalf("unrelated image blocked preview: %v", err)
	}
	for _, index := range []int{-1, 1, 2} {
		if _, err := PreviewInputImage(payload, index); err == nil {
			t.Fatalf("invalid image %d accepted", index)
		}
	}
}
