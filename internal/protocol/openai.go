package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// OpenAIImageRequest 是兼容层已经解析好的图像请求。Model 使用专家名。
type OpenAIImageRequest struct {
	Model          string
	Prompt         string
	N              *int
	Size           string
	ResponseFormat string
	Images         []InputImage
	GarmentName    string
	BBox           json.RawMessage
	BBoxFormat     string
	Gender         string
}

// ExpertCanvas 返回图像专家固定的输出画布，格式为宽x高。视频专家没有可由调用方指定的像素画布。
func ExpertCanvas(expert string) (string, bool) {
	switch expert {
	case ExpertVirtualTryOn:
		return "768x1024", true
	case ExpertClothingGeneration:
		return "1024x1024", true
	case ExpertBodyModel:
		return "1024x1536", true
	default:
		return "", false
	}
}

// TranslateOpenAIImage 把 OpenAI 图像请求转换成专家路由。文生图目前会被专家允许表拒绝。
func TranslateOpenAIImage(iface string, req OpenAIImageRequest, experimental bool) (ExpertRoute, error) {
	if err := checkOpenAICommon(req); err != nil {
		return ExpertRoute{}, err
	}
	if err := checkCanvas(req.Model, req.Size); err != nil {
		return ExpertRoute{}, err
	}
	if err := checkImageMIME(req.Images); err != nil {
		return ExpertRoute{}, err
	}
	prompt := strings.TrimSpace(req.Prompt)
	switch iface {
	case InterfaceImageGeneration:
		if prompt == "" {
			return ExpertRoute{}, errors.New("prompt is required")
		}
		if len(req.Images) != 0 || req.GarmentName != "" || len(req.BBox) != 0 || req.BBoxFormat != "" || req.Gender != "" {
			return ExpertRoute{}, errors.New("image generation only accepts a prompt")
		}
		body, err := json.Marshal(map[string]any{"prompt": prompt})
		if err != nil {
			return ExpertRoute{}, err
		}
		return ResolveExpert(iface, req.Model, body, experimental)
	case InterfaceImageEdit:
		return translateImageEdit(req, prompt, experimental)
	default:
		return ExpertRoute{}, errors.New("unsupported interface")
	}
}

// TranslateOpenAIVideo 把带首帧的视频请求转换成时尚 Motion。省略首帧的文生视频由专家路由拒绝。
func TranslateOpenAIVideo(req OpenAIImageRequest, experimental bool) (ExpertRoute, error) {
	if err := checkOpenAICommon(req); err != nil {
		return ExpertRoute{}, err
	}
	if req.Model != ExpertFashionMotion {
		if _, known := knownExpert(req.Model); !known {
			return ExpertRoute{}, errors.New("unsupported expert")
		}
		return ExpertRoute{}, errors.New("expert does not accept this interface")
	}
	if req.Size != "" {
		return ExpertRoute{}, errors.New("fashion_motion does not accept size")
	}
	if req.GarmentName != "" || len(req.BBox) != 0 || req.BBoxFormat != "" || req.Gender != "" {
		return ExpertRoute{}, errors.New("unsupported field")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return ExpertRoute{}, errors.New("prompt is required")
	}
	if err := checkImageMIME(req.Images); err != nil {
		return ExpertRoute{}, err
	}
	body, err := json.Marshal(map[string]any{"prompt": strings.TrimSpace(req.Prompt), "images": req.Images})
	if err != nil {
		return ExpertRoute{}, err
	}
	return ResolveExpert(InterfaceVideo, req.Model, body, experimental)
}

func translateImageEdit(req OpenAIImageRequest, prompt string, experimental bool) (ExpertRoute, error) {
	switch req.Model {
	case ExpertVirtualTryOn:
		if req.GarmentName != "" || len(req.BBox) != 0 || req.BBoxFormat != "" || req.Gender != "" {
			return ExpertRoute{}, errors.New("unsupported field")
		}
		if prompt == "" {
			return ExpertRoute{}, errors.New("prompt is required")
		}
		if len(req.Images) != 2 {
			return ExpertRoute{}, errors.New("virtual_tryon requires two images")
		}
		body, err := json.Marshal(map[string]any{"prompt": prompt, "images": req.Images})
		if err != nil {
			return ExpertRoute{}, err
		}
		return ResolveExpert(InterfaceImageEdit, req.Model, body, experimental)
	case ExpertClothingGeneration:
		if req.Gender != "" {
			return ExpertRoute{}, errors.New("unsupported field")
		}
		if strings.TrimSpace(req.GarmentName) == "" || utf8.RuneCountInString(req.GarmentName) > 128 || strings.ContainsAny(req.GarmentName, "\r\n\x00") {
			return ExpertRoute{}, errors.New("garment_name is required")
		}
		if len(req.Images) != 1 {
			return ExpertRoute{}, errors.New("clothing_generation requires one image")
		}
		if prompt != "" && len(req.BBox) == 0 {
			return ExpertRoute{}, errors.New("product generation does not accept prompt")
		}
		if prompt != "" && !experimental {
			return ExpertRoute{}, errors.New("clothing_generation does not accept prompt with the production credential")
		}
		fields := map[string]any{"garment_name": req.GarmentName, "images": req.Images}
		if len(req.BBox) != 0 {
			var coords []float64
			if json.Unmarshal(req.BBox, &coords) != nil || len(coords) != 4 {
				return ExpertRoute{}, errors.New("bbox must contain four coordinates")
			}
			fields["bbox"] = coords
		}
		if req.BBoxFormat != "" {
			if len(req.BBox) == 0 {
				return ExpertRoute{}, errors.New("bbox_format requires bbox")
			}
			switch req.BBoxFormat {
			case "auto", "normalized_xyxy", "pixel_xyxy":
			default:
				return ExpertRoute{}, errors.New("unsupported bbox_format")
			}
			fields["bbox_format"] = req.BBoxFormat
		}
		if prompt != "" {
			fields["prompt"] = prompt
		}
		body, err := json.Marshal(fields)
		if err != nil {
			return ExpertRoute{}, err
		}
		return ResolveExpert(InterfaceImageEdit, req.Model, body, experimental)
	case ExpertBodyModel:
		if req.GarmentName != "" || len(req.BBox) != 0 || req.BBoxFormat != "" {
			return ExpertRoute{}, errors.New("unsupported field")
		}
		switch req.Gender {
		case "male", "female", "unknown":
		default:
			return ExpertRoute{}, errors.New("gender must be male, female, or unknown")
		}
		if len(req.Images) != 1 {
			return ExpertRoute{}, errors.New("body_model requires one image")
		}
		if prompt != "" && !experimental {
			return ExpertRoute{}, errors.New("body_model does not accept prompt with the production credential")
		}
		fields := map[string]any{"gender": req.Gender, "images": req.Images}
		if prompt != "" {
			fields["prompt"] = prompt
		}
		body, err := json.Marshal(fields)
		if err != nil {
			return ExpertRoute{}, err
		}
		return ResolveExpert(InterfaceImageEdit, req.Model, body, experimental)
	case ExpertFashionMotion:
		return ExpertRoute{}, errors.New("expert does not accept this interface")
	default:
		return ExpertRoute{}, errors.New("unsupported expert")
	}
}

func checkOpenAICommon(req OpenAIImageRequest) error {
	if req.N != nil && *req.N != 1 {
		return errors.New("only n=1 is supported")
	}
	if req.ResponseFormat != "" && req.ResponseFormat != "b64_json" {
		return errors.New("response_format must be b64_json")
	}
	return nil
}

func checkCanvas(expert, size string) error {
	canvas, ok := ExpertCanvas(expert)
	if !ok {
		return nil
	}
	if size != "" && size != canvas {
		return fmt.Errorf("size must be %s", canvas)
	}
	return nil
}

func checkImageMIME(images []InputImage) error {
	for _, image := range images {
		switch image.MIMEType {
		case "image/png", "image/jpeg", "image/webp":
			if len(image.Data) == 0 {
				return errors.New("invalid image")
			}
		default:
			return errors.New("image must be png, jpeg, or webp")
		}
	}
	return nil
}

func knownExpert(expert string) (string, bool) {
	switch expert {
	case ExpertVirtualTryOn, ExpertClothingGeneration, ExpertBodyModel, ExpertFashionMotion:
		return expert, true
	default:
		return "", false
	}
}
