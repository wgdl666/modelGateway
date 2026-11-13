package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestValidationReturnsImagesOnlyOnSuccess(t *testing.T) {
	for _, validate := range []func(string, json.RawMessage) ([]InputImage, error){ValidateInput, ValidateExperimentalInput} {
		images, err := validate(ClothGenProtocol, json.RawMessage(`{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt"}`))
		if err != nil || len(images) != 1 || images[0].MIMEType != "image/png" || string(images[0].Data) != "x" {
			t.Fatalf("decoded images: %v, error: %v", images, err)
		}
		for _, input := range []string{
			`{"images":[{"mime_type":"image/png","data":"invalid!"}]}`,
			`{"images":[{"mime_type":"image/png","data":"eA=="}],"unexpected":"value"}`,
		} {
			images, err := validate(ClothGenProtocol, json.RawMessage(input))
			if err == nil || images != nil {
				t.Fatalf("invalid input returned reusable images: %v, error: %v", images, err)
			}
		}
	}
}

// 对照旧受理链路的二次解码，测量校验与清单生成这一段的分配，不代表端到端耗时。
func BenchmarkAdmissionInputDecode(b *testing.B) {
	input, err := json.Marshal(map[string]any{"images": []InputImage{{MIMEType: "image/png", Data: bytes.Repeat([]byte("x"), 1<<20)}}})
	if err != nil {
		b.Fatal(err)
	}
	for _, duplicate := range []bool{false, true} {
		name := "reuse"
		if duplicate {
			name = "duplicate"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				images, err := ValidateInput(ClothGenProtocol, input)
				if err != nil {
					b.Fatal(err)
				}
				if duplicate {
					_, images, err = DecodeInput(input)
					if err != nil {
						b.Fatal(err)
					}
				}
				if _, err := InputMediaExtra(InputMedia(ClothGenProtocol, images)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
