package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWardrobeRoutesAreSelectedByModelKey(t *testing.T) {
	const product = `{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt"}`
	const extraction = `{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt","bbox":[0,0,1,1]}`
	for _, test := range []struct{ model, input, want string }{
		{clothGenModel, product, "cloth_gen"},
		{wardrobeModel, extraction, "garment_extraction"},
		{wardrobeModel, product, ""},
		{clothGenModel, extraction, ""},
	} {
		got, err := Route(test.model, json.RawMessage(test.input))
		if got != test.want || (err == nil) != (test.want != "") {
			t.Errorf("Route(%s, %s) = %q, %v, want %q", test.model, test.input, got, err, test.want)
		}
	}
}

func TestInstanceRegistrationRejectsMismatchedModelCapability(t *testing.T) {
	i := Instance{
		ID: "product-1", ModelKey: wardrobeModel, Capability: "cloth_gen",
		Artifact: "sha256-" + strings.Repeat("a", 64), ResourceGroup: "gpu0", Capacity: 1,
	}
	if i.Valid() {
		t.Fatal("registered a product generation worker under the extraction model key")
	}
	i.ModelKey = clothGenModel
	if !i.Valid() {
		t.Fatal("product generation registration rejected its own model key")
	}
}
