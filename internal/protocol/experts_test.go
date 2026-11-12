package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExpertRoutesStayOnRegisteredModels(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	tryOn := `{"images":[` + image + `,` + image + `],"prompt":"try on"}`
	product := `{"images":[` + image + `],"garment_name":"shirt"}`
	extraction := `{"images":[` + image + `],"garment_name":"shirt","bbox":[0,0,1,1]}`
	body := `{"images":[` + image + `],"gender":"female"}`
	frame := `{"images":[` + image + `],"prompt":"walk","motion_tips":"slow turn"}`
	for _, test := range []struct {
		iface, expert, input, model, kind string
	}{
		{InterfaceImageEdit, ExpertVirtualTryOn, tryOn, OutfitModel, OutfitProtocol},
		{InterfaceImageEdit, ExpertClothingGeneration, product, ClothGenModel, ClothGenProtocol},
		{InterfaceImageEdit, ExpertClothingGeneration, extraction, WardrobeModel, WardrobeProtocol},
		{InterfaceImageEdit, ExpertBodyModel, body, BodyModel, BodyProtocol},
		{InterfaceVideo, ExpertFashionMotion, frame, VideoModel, VideoProtocol},
	} {
		route, err := ResolveExpert(test.iface, test.expert, json.RawMessage(test.input), false)
		if err != nil || route.ModelKey != test.model || route.Protocol != test.kind || route.Expert != test.expert {
			t.Errorf("ResolveExpert(%s, %s) = %+v, %v", test.iface, test.expert, route, err)
		}
		if ExpertForModel(route.ModelKey) != test.expert {
			t.Errorf("expert for %s = %s", route.ModelKey, ExpertForModel(route.ModelKey))
		}
	}
}

func TestClothingGenerationSelectsExtractionFromUnifiedText(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	input := `{"images":[` + image + `],"text":"{\"garment_name\":\"shirt\",\"bbox\":[0,0,1,1]}"}`
	route, err := ResolveExpert(InterfaceImageEdit, ExpertClothingGeneration, json.RawMessage(input), false)
	if err != nil || route.Protocol != WardrobeProtocol || route.ModelKey != WardrobeModel {
		t.Fatalf("unified extraction route = %+v, %v", route, err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(route.Input, &fields) != nil || fields["bbox"] == nil || fields["text"] != nil {
		t.Fatalf("normalized input = %s", route.Input)
	}
}

func TestExpertRouteRejectsDisallowedCombinations(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	prompt := `{"images":[` + image + `],"prompt":"a coat"}`
	for _, test := range []struct {
		iface, expert, input, want string
	}{
		{InterfaceImageGeneration, ExpertVirtualTryOn, `{"prompt":"a coat"}`, "image generation has no expert"},
		{InterfaceImageGeneration, ExpertBodyModel, `{"prompt":"a coat"}`, "image generation has no expert"},
		{InterfaceImageEdit, ExpertFashionMotion, prompt, "expert does not accept this interface"},
		{InterfaceVideo, ExpertVirtualTryOn, prompt, "expert does not accept this interface"},
		{InterfaceVideo, ExpertFashionMotion, `{"prompt":"walk"}`, "text-to-video is not available"},
		{InterfaceVideo, ExpertFashionMotion, `{"prompt":"walk","images":[]}`, "text-to-video is not available"},
		{InterfaceVideo, ExpertFashionMotion, `{"images":[` + image + `,` + image + `],"prompt":"walk"}`, "one first frame"},
		{"audio", ExpertBodyModel, prompt, "unsupported interface"},
		{InterfaceImageEdit, "general", prompt, "unsupported expert"},
	} {
		_, err := ResolveExpert(test.iface, test.expert, json.RawMessage(test.input), false)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("ResolveExpert(%s, %s) error = %v, want %q", test.iface, test.expert, err, test.want)
		}
	}
}

func TestExpertExperimentalFieldsFollowTheExistingCredential(t *testing.T) {
	const image = `{"mime_type":"image/png","data":"eA=="}`
	body := `{"images":[` + image + `],"gender":"female","prompt":"studio light"}`
	if _, err := ResolveExpert(InterfaceImageEdit, ExpertBodyModel, json.RawMessage(body), false); err == nil {
		t.Fatal("production credential accepted a body prompt override")
	}
	route, err := ResolveExpert(InterfaceImageEdit, ExpertBodyModel, json.RawMessage(body), true)
	if err != nil || route.ModelKey != BodyModel {
		t.Fatalf("experimental body route = %+v, %v", route, err)
	}
}
