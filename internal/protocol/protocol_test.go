package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

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
		Artifact: "sha256-" + strings.Repeat("a", 64), ResourceGroup: "gpu0", Capacity: 1, GroupCapacity: 1,
	}
	if i.Valid() {
		t.Fatal("registered an instance without a valid model key")
	}
	i.ModelKey = ClothGenModel
	if !i.Valid() {
		t.Fatal("product generation registration rejected its own model key")
	}
}
