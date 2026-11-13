package worker

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"testing"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestNativeMultipartStreamsMediaAndReplays(t *testing.T) {
	for _, edits := range []bool{false, true} {
		original := []protocol.InputImage{{MIMEType: "image/png", Data: bytes.Repeat([]byte("a"), 1<<20)}, {MIMEType: "image/jpeg", Data: []byte("second")}}
		raw, err := json.Marshal(map[string]any{"images": original, "prompt": "test"})
		if err != nil {
			t.Fatal(err)
		}
		path, body, headers, err := nativeMultipart(&ent.Request{ID: "request", Protocol: protocol.OutfitProtocol}, protocol.Submission{Input: raw}, config.NativeBackend{VTONEdits: edits})
		if err != nil {
			t.Fatal(err)
		}
		if edits && path != fluxEditsPath {
			t.Fatal(path)
		}
		// 每张图片保留为单独片段，表单缓冲不包含媒体正文。
		if len(body.chunks) != 5 || len(body.chunks[0]) > 4096 || len(body.chunks[2]) > 4096 || len(body.chunks[4]) > 4096 {
			t.Fatal("media copied into framing")
		}
		encoded, err := io.ReadAll(body)
		if err != nil || int64(len(encoded)) != body.length {
			t.Fatalf("length: %d %d %v", len(encoded), body.length, err)
		}
		replay := body.replay()
		again, err := io.ReadAll(replay)
		replay.Close()
		if err != nil || !bytes.Equal(encoded, again) {
			t.Fatal("replay differs")
		}
		_, params, err := mime.ParseMediaType(headers.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		reader := multipart.NewReader(bytes.NewReader(encoded), params["boundary"])
		files := 0
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			if part.FileName() != "" {
				if files >= len(original) || !bytes.Equal(data, original[files].Data) || part.Header.Get("Content-Type") != original[files].MIMEType {
					t.Fatal("file changed")
				}
				want := []string{"person_image", "garment_image"}[files]
				if edits {
					want = "image"
				}
				if part.FormName() != want {
					t.Fatal("file field changed")
				}
				files++
			} else if part.FormName() == "prompt" && string(data) != "test" {
				t.Fatal("field changed")
			}
		}
		if files != 2 {
			t.Fatal("missing files")
		}
	}
}
