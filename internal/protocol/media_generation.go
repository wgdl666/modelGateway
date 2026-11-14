package protocol

import (
	"encoding/json"
	"errors"
	"strings"
)

const (
	ImageGenerationProtocol = "image_generation"
	VideoGenerationProtocol = "video_generation"
)

// Generation protocols describe the output medium, never a business task or model family.
func GenerationProtocol(kind string) bool {
	return kind == ImageGenerationProtocol || kind == VideoGenerationProtocol
}

func VideoOutput(kind string) bool {
	return kind == VideoProtocol || kind == VideoGenerationProtocol
}

// Parameters is a model-owned JSON object. The gateway neither interprets its
// keys nor changes their types, and never merges them into routing fields.
type ImageGenerationInput struct {
	Prompt         string          `json:"prompt,omitempty"`
	Images         []InputImage    `json:"images,omitempty"`
	N              *int            `json:"n,omitempty"`
	Size           string          `json:"size,omitempty"`
	ResponseFormat string          `json:"response_format,omitempty"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
}

type VideoGenerationInput struct {
	Prompt     string          `json:"prompt,omitempty"`
	Images     []InputImage    `json:"images,omitempty"`
	Size       string          `json:"size,omitempty"`
	Seconds    *float64        `json:"seconds,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

type ImageGenerationRequest struct {
	Model string `json:"model"`
	ImageGenerationInput
	User string `json:"user,omitempty"`
}

type VideoGenerationRequest struct {
	Model string `json:"model"`
	VideoGenerationInput
	User string `json:"user,omitempty"`
}

func ValidateGenerationInput(kind string, raw json.RawMessage) error {
	switch kind {
	case ImageGenerationProtocol:
		var input ImageGenerationInput
		if err := DecodeJSON(raw, &input); err != nil {
			return errors.New("invalid image generation input")
		}
		if input.N != nil && *input.N < 1 {
			return errors.New("n must be positive")
		}
		if input.ResponseFormat != "" && input.ResponseFormat != "b64_json" {
			return errors.New("response_format must be b64_json")
		}
		return validateGeneration(input.Prompt, input.Images, input.Parameters)
	case VideoGenerationProtocol:
		var input VideoGenerationInput
		if err := DecodeJSON(raw, &input); err != nil {
			return errors.New("invalid video generation input")
		}
		if input.Seconds != nil && *input.Seconds <= 0 {
			return errors.New("seconds must be positive")
		}
		return validateGeneration(input.Prompt, input.Images, input.Parameters)
	default:
		return errors.New("unsupported generation protocol")
	}
}

func validateGeneration(prompt string, images []InputImage, parameters json.RawMessage) error {
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		return errors.New("prompt or reference images are required")
	}
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
	if len(parameters) != 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(parameters, &fields) != nil || fields == nil {
			return errors.New("parameters must be a JSON object")
		}
	}
	return nil
}
