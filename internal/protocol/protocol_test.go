package protocol

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestPreviewKeepsBusinessFieldsAndImageRoles(t *testing.T) {
	id := "d36fa74f-14db-4a32-9315-8307dcfc0bcd"
	payload := json.RawMessage(`{"input":{"prompt":"try on","images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/jpeg","data":"eA=="}]},"metadata":{"trace_id":"trace"}}`)
	parameters, refs, images, err := PreviewSubmission(OutfitProtocol, payload)
	if err != nil || len(images) != 2 || string(images[0].Data) != "x" {
		t.Fatalf("preview images: %v %v", images, err)
	}
	if strings.Contains(string(parameters), "eA==") || strings.Contains(string(parameters), "images") {
		t.Fatalf("parameters contain image bytes: %s", parameters)
	}
	var fields map[string]string
	if json.Unmarshal(parameters, &fields) != nil || fields["prompt"] != "try on" || len(fields) != 1 {
		t.Fatalf("parameters = %s", parameters)
	}
	if len(refs) != 2 || refs[0].Role != "person_image" || refs[1].Role != "garment_image" || refs[1].MIMEType != "image/jpeg" {
		t.Fatalf("roles = %+v", refs)
	}
	key := "outputs/" + id + "/0.png"
	if !MatchesOutputKey(id, 0, key) || OutputMIME(key) != "image/png" || MatchesOutputKey(id, 1, key) || MatchesOutputKey(id, 0, id+"/result") || MatchesOutputKey(id, 1, "outputs/"+id+"/10.png") {
		t.Fatalf("output key check failed for %s", key)
	}
	files := OutputMedia([]Object{{Key: key}, {Key: id + "/result"}, {Key: "outputs/" + id + "/0.mp4"}})
	if len(files) != 2 || files[0].Index != 0 || files[1].Index != 2 || files[1].MIMEType != "video/mp4" {
		t.Fatalf("output media = %+v", files)
	}
}

func TestInputMediaRecordsReadableImageSize(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1536, 2048))); err != nil {
		t.Fatal(err)
	}
	refs := InputMedia(OutfitProtocol, []InputImage{{MIMEType: "image/png", Data: buf.Bytes()}, {MIMEType: "image/png", Data: []byte("x")}})
	if refs[0].Width != 1536 || refs[0].Height != 2048 || refs[0].Role != "person_image" {
		t.Fatalf("person ref = %+v", refs[0])
	}
	// 受理不校验图片内容，读不出尺寸的图仍要列出，只是不带宽高。
	if refs[1].Width != 0 || refs[1].Height != 0 || refs[1].Role != "garment_image" {
		t.Fatalf("garment ref = %+v", refs[1])
	}
	raw, _ := json.Marshal(refs[1])
	if strings.Contains(string(raw), "width") {
		t.Fatalf("unknown size must be omitted: %s", raw)
	}
}

func TestWardrobeRoutesAreSelectedByProtocol(t *testing.T) {
	const product = `{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt"}`
	const extraction = `{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt","bbox":[0,0,1,1]}`
	for _, test := range []struct {
		model, input string
		valid        bool
	}{
		{ClothGenProtocol, product, true},
		{WardrobeProtocol, extraction, true},
		{WardrobeProtocol, product, false},
		{ClothGenProtocol, extraction, false},
	} {
		if err := ValidateInput(test.model, json.RawMessage(test.input)); (err == nil) != test.valid {
			t.Errorf("ValidateInput(%s, %s) = %v, want valid=%v", test.model, test.input, err, test.valid)
		}
	}
}

func TestExperimentalFieldsRequireExperimentalCredential(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	for _, test := range []struct {
		kind, input  string
		experimental bool
	}{
		{WardrobeProtocol, `{"images":[` + image + `],"garment_name":"shirt","bbox":[0,0,1,1],"prompt":"keep the logo","use_pe":"false"}`, true},
		{BodyProtocol, `{"images":[` + image + `],"gender":"female","prompt":"studio light"}`, true},
		{OutfitProtocol, `{"images":[` + image + `,` + image + `],"prompt":"try on","use_pe":"false"}`, true},
		{VideoProtocol, `{"images":[` + image + `],"prompt":"walk","model":"ltx","duration":"4","resolution":"720p","fps":"24","audio":"false"}`, true},
		{WardrobeProtocol, `{"images":[` + image + `],"garment_name":"shirt","bbox":[0,0,1,1]}`, false},
	} {
		if err := ValidateExperimentalInput(test.kind, json.RawMessage(test.input)); err != nil {
			t.Errorf("experimental %s rejected %s: %v", test.kind, test.input, err)
		}
		if err := ValidateInput(test.kind, json.RawMessage(test.input)); (err == nil) == test.experimental {
			t.Errorf("production %s: %s accepted=%v, want %v", test.kind, test.input, err == nil, !test.experimental)
		}
	}
	for _, input := range []string{
		`{"images":[` + image + `],"gender":"female","use_pe":"false"}`,
		`{"images":[` + image + `],"gender":"female","seed":"42"}`,
		`{"images":[` + image + `],"gender":"female","prompt":{"text":"x"}}`,
		`{"images":[` + image + `],"gender":"female","prompt":null}`,
	} {
		if err := ValidateExperimentalInput(BodyProtocol, json.RawMessage(input)); err == nil {
			t.Errorf("experimental body accepted %s", input)
		}
	}
	if err := ValidateExperimentalInput(ClothGenProtocol, json.RawMessage(`{"images":[`+image+`],"garment_name":"shirt","prompt":"x"}`)); err == nil {
		t.Error("product generation has no experimental fields")
	}
}

func TestInstanceRegistrationUsesProtocolForNewModelKeys(t *testing.T) {
	i := Instance{
		ID: "product-1", ModelKey: "cloth_gen",
		Artifact: "sha256-" + strings.Repeat("a", 64), ResourceGroup: "gpu0", Capacity: 1,
	}
	if i.Valid() {
		t.Fatal("registered an instance without a supported protocol")
	}
	i.Protocol = ClothGenProtocol
	if !i.Valid() {
		t.Fatal("new model key with product protocol was rejected")
	}
}

func TestUnknownProtocolAndInvalidModelKeyAreRejected(t *testing.T) {
	i := Instance{ID: "instance-1", ModelKey: "new-model-v2", Protocol: "unsupported", ResourceGroup: "gpu0", Capacity: 1}
	if i.Valid() {
		t.Fatal("unknown protocol accepted")
	}
	i.Protocol = OutfitProtocol
	i.ModelKey = "../other-model"
	if i.Valid() {
		t.Fatal("invalid model identifier accepted")
	}
}
