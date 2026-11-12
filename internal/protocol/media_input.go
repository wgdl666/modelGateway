package protocol

import (
	"encoding/json"
	"errors"
)

// NormalizeMediaInput 在网关内把 ModelHub 的统一输入转换为已登记协议的业务字段。
// 原有直接调用仍可提交扁平字段；落队列的始终是同一份原生输入，派发与重放无需再分支。
func NormalizeMediaInput(kind string, input json.RawMessage) (json.RawMessage, error) {
	if GenerationProtocol(kind) {
		return input, nil
	}
	var source map[string]json.RawMessage
	if json.Unmarshal(input, &source) != nil || source == nil {
		return nil, errors.New("input must be a JSON object")
	}
	textRaw, unified := source["text"]
	if !unified {
		return input, nil
	}
	for key := range source {
		if key != "images" && key != "text" && key != "model_traits" {
			return nil, errors.New("unsupported image input field")
		}
	}
	var text string
	if json.Unmarshal(textRaw, &text) != nil {
		return nil, errors.New("image text must be a string")
	}
	fields := map[string]json.RawMessage{}
	switch kind {
	case OutfitProtocol:
		fields["prompt"] = textRaw
	case WardrobeProtocol, ClothGenProtocol:
		var garment struct {
			Name        string            `json:"garment_name,omitempty"`
			Role        string            `json:"garment_role,omitempty"`
			BBox        []float64         `json:"bbox,omitempty"`
			BBoxFormat  string            `json:"bbox_format,omitempty"`
			ParseLabels map[string]string `json:"parse_labels,omitempty"`
		}
		if json.Unmarshal([]byte(text), &garment) != nil {
			return nil, errors.New("garment text must be JSON metadata")
		}
		if kind == ClothGenProtocol {
			if garment.BBox != nil || garment.ParseLabels != nil || garment.BBoxFormat != "" {
				return nil, errors.New("product generation cannot accept garment extraction fields")
			}
		}
		fields["garment_name"], _ = json.Marshal(garment.Name)
		if garment.Role != "" {
			fields["garment_role"], _ = json.Marshal(garment.Role)
		}
		if garment.BBox != nil {
			fields["bbox"], _ = json.Marshal(garment.BBox)
			if garment.BBoxFormat != "" {
				fields["bbox_format"], _ = json.Marshal(garment.BBoxFormat)
			}
		}
		if garment.ParseLabels != nil {
			fields["parse_labels"], _ = json.Marshal(garment.ParseLabels)
		}
	case BodyProtocol:
		var traits struct {
			Gender string `json:"gender"`
		}
		if raw := source["model_traits"]; len(raw) > 0 && json.Unmarshal(raw, &traits) != nil {
			return nil, errors.New("body model_traits must contain a string gender")
		}
		fields["gender"], _ = json.Marshal(traits.Gender)
	default:
		return nil, errors.New("model does not use the image input protocol")
	}
	fields["images"] = source["images"]
	return json.Marshal(fields)
}
