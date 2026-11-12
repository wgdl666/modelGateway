package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func png() InputImage { return InputImage{MIMEType: "image/png", Data: []byte("x")} }

func TestOpenAIEditRoutesToTheExpert(t *testing.T) {
	tryOn, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{
		Model: ExpertVirtualTryOn, Prompt: "try on", Size: "768x1024", Images: []InputImage{png(), png()},
	}, false)
	if err != nil || tryOn.ModelKey != OutfitModel || tryOn.Protocol != OutfitProtocol {
		t.Fatalf("try-on route = %+v, %v", tryOn, err)
	}
	product, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{
		Model: ExpertClothingGeneration, GarmentName: "shirt", Images: []InputImage{png()},
	}, false)
	if err != nil || product.ModelKey != ClothGenModel {
		t.Fatalf("product route = %+v, %v", product, err)
	}
	extraction, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{
		Model: ExpertClothingGeneration, GarmentName: "shirt", BBox: json.RawMessage(`[0,0,1,1]`),
		Prompt: "keep the logo", Images: []InputImage{png()},
	}, true)
	if err != nil || extraction.ModelKey != WardrobeModel || !strings.Contains(string(extraction.Input), "keep the logo") {
		t.Fatalf("extraction route = %+v, %v", extraction, err)
	}
	body, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{
		Model: ExpertBodyModel, Gender: "female", Size: "1024x1536", Images: []InputImage{png()},
	}, false)
	if err != nil || body.ModelKey != BodyModel {
		t.Fatalf("body route = %+v, %v", body, err)
	}
	video, err := TranslateOpenAIVideo(OpenAIImageRequest{
		Model: ExpertFashionMotion, Prompt: "walk", Images: []InputImage{png()},
	}, false)
	if err != nil || video.ModelKey != VideoModel || video.Protocol != VideoProtocol {
		t.Fatalf("video route = %+v, %v", video, err)
	}
}

func TestOpenAICompatibilityRejectsUnsupportedShapes(t *testing.T) {
	two := 2
	for _, test := range []struct {
		name string
		run  func() error
		want string
	}{
		{"generation", func() error {
			_, err := TranslateOpenAIImage(InterfaceImageGeneration, OpenAIImageRequest{Model: ExpertVirtualTryOn, Prompt: "a coat"}, false)
			return err
		}, "image generation has no expert"},
		{"size", func() error {
			_, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{Model: ExpertVirtualTryOn, Prompt: "try on", Size: "1024x1024", Images: []InputImage{png(), png()}}, false)
			return err
		}, "size must be 768x1024"},
		{"count", func() error {
			_, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{Model: ExpertBodyModel, Gender: "male", N: &two, Images: []InputImage{png()}}, false)
			return err
		}, "only n=1"},
		{"format", func() error {
			_, err := TranslateOpenAIImage(InterfaceImageGeneration, OpenAIImageRequest{Model: ExpertBodyModel, Prompt: "a coat", ResponseFormat: "url"}, false)
			return err
		}, "b64_json"},
		{"product prompt", func() error {
			_, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{Model: ExpertClothingGeneration, GarmentName: "shirt", Prompt: "brighter", Images: []InputImage{png()}}, true)
			return err
		}, "product generation does not accept prompt"},
		{"body prompt", func() error {
			_, err := TranslateOpenAIImage(InterfaceImageEdit, OpenAIImageRequest{Model: ExpertBodyModel, Gender: "unknown", Prompt: "studio", Images: []InputImage{png()}}, false)
			return err
		}, "production credential"},
		{"text video", func() error {
			_, err := TranslateOpenAIVideo(OpenAIImageRequest{Model: ExpertFashionMotion, Prompt: "walk"}, false)
			return err
		}, "text-to-video is not available"},
		{"video size", func() error {
			_, err := TranslateOpenAIVideo(OpenAIImageRequest{Model: ExpertFashionMotion, Prompt: "walk", Size: "720x1280", Images: []InputImage{png()}}, false)
			return err
		}, "does not accept size"},
	} {
		err := test.run()
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s error = %v, want %q", test.name, err, test.want)
		}
	}
}
