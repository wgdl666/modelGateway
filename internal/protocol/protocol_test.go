package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPreviewKeepsBusinessFieldsAndImageRoles(t *testing.T) {
	id := "d36fa74f-14db-4a32-9315-8307dcfc0bcd"
	payload := json.RawMessage(`{"input":{"prompt":"try on","images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/jpeg","data":"eA=="}]},"metadata":{"trace_id":"trace"}}`)
	parameters, refs, images, err := PreviewSubmission(OutfitModel, payload)
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

func TestWardrobeRoutesAreSelectedByModelKey(t *testing.T) {
	const product = `{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt"}`
	const extraction = `{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt","bbox":[0,0,1,1]}`
	for _, test := range []struct {
		model, input string
		valid        bool
	}{
		{ClothGenModel, product, true},
		{WardrobeModel, extraction, true},
		{WardrobeModel, product, false},
		{ClothGenModel, extraction, false},
	} {
		if err := ValidateInput(test.model, json.RawMessage(test.input)); (err == nil) != test.valid {
			t.Errorf("ValidateInput(%s, %s) = %v, want valid=%v", test.model, test.input, err, test.valid)
		}
	}
}

func TestInstanceRegistrationRequiresKnownModelKey(t *testing.T) {
	i := Instance{
		ID: "product-1", ModelKey: "cloth_gen",
		Artifact: "sha256-" + strings.Repeat("a", 64), ResourceGroup: "gpu0", Capacity: 1,
	}
	if i.Valid() {
		t.Fatal("registered an instance without a valid model key")
	}
	i.ModelKey = ClothGenModel
	if !i.Valid() {
		t.Fatal("product generation registration rejected its own model key")
	}
}
