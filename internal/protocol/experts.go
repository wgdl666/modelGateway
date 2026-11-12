package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
)

const (
	InterfaceImageGeneration = "image_generation"
	InterfaceImageEdit       = "image_edit"
	InterfaceVideo           = "video"

	ExpertVirtualTryOn       = "virtual_tryon"
	ExpertClothingGeneration = "clothing_generation"
	ExpertBodyModel          = "body_model"
	ExpertFashionMotion      = "fashion_motion"
)

// ExpertRoute 是专家路由的结果。ModelKey 和 Protocol 都指向已经登记的业务工人，不在进程内切换权重。
type ExpertRoute struct {
	Expert    string
	Interface string
	ModelKey  string
	Protocol  string
	Input     json.RawMessage
}

// ExpertForModel 从已经派到的模型键还原专家名。直接按模型键提交的旧请求也使用同一张表。
func ExpertForModel(model string) string {
	switch model {
	case OutfitModel:
		return ExpertVirtualTryOn
	case WardrobeModel, ClothGenModel:
		return ExpertClothingGeneration
	case BodyModel:
		return ExpertBodyModel
	case VideoModel:
		return ExpertFashionMotion
	default:
		return ""
	}
}

// ResolveExpert 在受理进队列之前检查接口和专家的组合，并转换成现有协议的输入。
// 文生图目前没有专家。时尚 Motion 的文生视频不派给只要首帧的视频工人。
func ResolveExpert(iface, expert string, input json.RawMessage, experimental bool) (ExpertRoute, error) {
	route := ExpertRoute{Expert: expert, Interface: iface}
	switch expert {
	case ExpertVirtualTryOn, ExpertClothingGeneration, ExpertBodyModel, ExpertFashionMotion:
	default:
		return ExpertRoute{}, errors.New("unsupported expert")
	}
	switch iface {
	case InterfaceImageGeneration:
		return ExpertRoute{}, errors.New("image generation has no expert; virtual_tryon, clothing_generation and body_model accept image editing")
	case InterfaceImageEdit, InterfaceVideo:
	default:
		return ExpertRoute{}, errors.New("unsupported interface")
	}
	if (iface == InterfaceVideo) != (expert == ExpertFashionMotion) {
		return ExpertRoute{}, errors.New("expert does not accept this interface")
	}

	var kind, model string
	switch expert {
	case ExpertVirtualTryOn:
		kind, model = OutfitProtocol, OutfitModel
	case ExpertBodyModel:
		kind, model = BodyProtocol, BodyModel
	case ExpertFashionMotion:
		count, err := countInputImages(input)
		if err != nil {
			return ExpertRoute{}, err
		}
		if count == 0 {
			return ExpertRoute{}, errors.New("text-to-video is not available for fashion_motion; submit one first frame")
		}
		if count != 1 {
			return ExpertRoute{}, errors.New("fashion_motion accepts one first frame")
		}
		kind, model = VideoProtocol, VideoModel
	case ExpertClothingGeneration:
		var err error
		kind, err = clothingProtocol(input)
		if err != nil {
			return ExpertRoute{}, err
		}
		if kind == WardrobeProtocol {
			model = WardrobeModel
		} else {
			model = ClothGenModel
		}
	}
	normalized, err := NormalizeMediaInput(kind, input)
	if err != nil {
		return ExpertRoute{}, err
	}
	validate := ValidateInput
	if experimental {
		validate = ValidateExperimentalInput
	}
	if err := validate(kind, normalized); err != nil {
		return ExpertRoute{}, err
	}
	route.Protocol = kind
	route.ModelKey = model
	route.Input = normalized
	return route, nil
}

func clothingProtocol(input json.RawMessage) (string, error) {
	var source map[string]json.RawMessage
	if json.Unmarshal(input, &source) != nil || source == nil {
		return "", errors.New("input must be a JSON object")
	}
	if textRaw, unified := source["text"]; unified {
		var text string
		if json.Unmarshal(textRaw, &text) != nil {
			return "", errors.New("image text must be a string")
		}
		var garment struct {
			BBox        []float64         `json:"bbox"`
			ParseLabels map[string]string `json:"parse_labels"`
		}
		if json.Unmarshal([]byte(text), &garment) != nil {
			return "", errors.New("garment text must be JSON metadata")
		}
		if garment.BBox != nil || garment.ParseLabels != nil {
			return WardrobeProtocol, nil
		}
		return ClothGenProtocol, nil
	}
	if source["bbox"] != nil || source["parse_labels"] != nil {
		return WardrobeProtocol, nil
	}
	return ClothGenProtocol, nil
}

func countInputImages(input json.RawMessage) (int, error) {
	var source map[string]json.RawMessage
	if json.Unmarshal(input, &source) != nil || source == nil {
		return 0, errors.New("input must be a JSON object")
	}
	raw, ok := source["images"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, nil
	}
	var images []InputImage
	if json.Unmarshal(raw, &images) != nil {
		return 0, errors.New("images must contain mime_type and base64 data")
	}
	for _, image := range images {
		if !imageMIME.MatchString(image.MIMEType) || len(image.Data) == 0 {
			return 0, errors.New("invalid image")
		}
	}
	return len(images), nil
}
