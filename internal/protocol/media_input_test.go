package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestUnifiedImageInputUsesRegisteredProtocol(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	for _, tc := range []struct {
		name, kind, text, traits string
		count                    int
		want                     map[string]any
		invalid                  bool
	}{
		{name: "tryon", kind: OutfitProtocol, text: " preserve prompt ", count: 2, want: map[string]any{"prompt": " preserve prompt "}},
		{name: "body", kind: BodyProtocol, text: "ignored business prompt", traits: `{"gender":"female"}`, count: 1, want: map[string]any{"gender": "female"}},
		{name: "garment", kind: WardrobeProtocol, text: `{"garment_name":"shirt","garment_role":"tops","bbox":[1,2,3,4],"bbox_format":"xyxy"}`, count: 1,
			want: map[string]any{"garment_name": "shirt", "garment_role": "tops", "bbox": []any{float64(1), float64(2), float64(3), float64(4)}, "bbox_format": "xyxy"}},
		{name: "mask", kind: WardrobeProtocol, text: `{"garment_name":"shirt","parse_labels":{}}`, count: 2, want: map[string]any{"garment_name": "shirt", "parse_labels": map[string]any{}}},
		{name: "product", kind: ClothGenProtocol, text: `{"garment_name":"shirt"}`, count: 1, want: map[string]any{"garment_name": "shirt"}},
		{name: "missing extraction region", kind: WardrobeProtocol, text: `{"garment_name":"shirt"}`, count: 1, invalid: true},
		{name: "wrong image count", kind: OutfitProtocol, text: "try on", count: 1, invalid: true},
		{name: "product cannot become extraction", kind: ClothGenProtocol, text: `{"bbox":[1,2,3,4]}`, count: 1, invalid: true},
		{name: "wrong traits type", kind: BodyProtocol, traits: `{"gender":true}`, count: 1, invalid: true},
		{name: "not image protocol", kind: VideoProtocol, text: "video", count: 1, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			images := make([]json.RawMessage, tc.count)
			for i := range images {
				images[i] = json.RawMessage(image)
			}
			input := map[string]any{"images": images, "text": tc.text}
			if tc.traits != "" {
				input["model_traits"] = json.RawMessage(tc.traits)
			}
			raw, _ := json.Marshal(input)
			normalized, err := NormalizeMediaInput(tc.kind, raw)
			if err == nil {
				err = ValidateInput(tc.kind, normalized)
			}
			if (err != nil) != tc.invalid {
				t.Fatalf("normalization = %s, error = %v", normalized, err)
			}
			if tc.invalid {
				return
			}
			var fields map[string]any
			_ = json.Unmarshal(normalized, &fields)
			if len(fields["images"].([]any)) != tc.count {
				t.Fatal("input image order/count changed")
			}
			delete(fields, "images")
			if !reflect.DeepEqual(fields, tc.want) {
				t.Fatalf("fields = %#v, want %#v", fields, tc.want)
			}
		})
	}
}
