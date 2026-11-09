package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

const (
	WardrobeModel = "Vmind_wardrobe_v1.0"
	ClothGenModel = "Vmind_cloth_gen_v1.0"
	OutfitModel   = "Vmind_outfit_v1.0"
	BodyModel     = "Vmind_bodymodel_v1.0"
	VideoModel    = "Vmind_outfit_video_v1.0"
)

var (
	imageMIME  = regexp.MustCompile(`^image/[A-Za-z0-9.+-]+$`)
	instanceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,255}$`)
)

// Instance 记录部署身份和同卡分组。是否可派由主动探针及排空状态决定。
// 原生视频同时登记该分组对应的 LTX 目标。
type Instance struct {
	ID             string `json:"instance_id"`
	ModelKey       string `json:"model_key"`
	Endpoint       string `json:"endpoint"`
	HealthURL      string `json:"health_url"`
	Artifact       string `json:"artifact_digest"`
	ResourceGroup  string `json:"resource_group"`
	TargetInstance string `json:"target_instance,omitempty"`
	GroupCapacity  int    `json:"group_capacity_units"`
	Capacity       int    `json:"capacity_units"`
	MachineID      string `json:"machine_id,omitempty"`
	GPUDevices     []int  `json:"gpu_devices,omitempty"`
}

// Valid 检查部署身份和容量。地址还需按当前配置的出站白名单验证。
func (i Instance) Valid() bool {
	return ValidInstanceID(i.ID) && ValidModelKey(i.ModelKey) &&
		i.ResourceGroup != "" && len(i.ResourceGroup) <= 256 && validArtifact(i.Artifact) &&
		i.Capacity >= 1 && i.Capacity <= 65536 && i.GroupCapacity >= 1 && i.GroupCapacity <= 65536
}

func ValidInstanceID(id string) bool {
	return instanceID.MatchString(id)
}

type Metadata struct {
	UserID  string `json:"user_id,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
}

type Submission struct {
	Input    json.RawMessage `json:"input"`
	Metadata Metadata        `json:"metadata,omitempty"`
}

type ModelRequest struct {
	RequestID string `json:"request_id"`
	ModelKey  string `json:"model_key"`
	Submission
}

type Object struct {
	Provider string `json:"provider"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
}

type Result struct {
	Files []Object `json:"files"`
}

type RequestError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Response struct {
	RequestID string        `json:"request_id"`
	ModelKey  string        `json:"model_key,omitempty"`
	State     string        `json:"state"`
	Result    *Result       `json:"result,omitempty"`
	Error     *RequestError `json:"error,omitempty"`
}

type ModelResponse struct {
	Response
	BackendTaskID string `json:"backend_task_id,omitempty"`
}

func (r ModelResponse) Validate(id string) error {
	if r.RequestID != id {
		return errors.New("model returned another request ID")
	}
	switch r.State {
	case "processing":
		if r.Result == nil && r.Error == nil {
			return nil
		}
	case "failed":
		if r.Error != nil && r.Error.Code != "" && r.Result == nil {
			return nil
		}
	case "succeeded":
		if r.Result != nil && len(r.Result.Files) > 0 && r.Error == nil {
			for _, file := range r.Result.Files {
				if (file.Provider != "s3" && file.Provider != "oss") || file.Region == "" || file.Bucket == "" || file.Key == "" {
					return errors.New("invalid object reference")
				}
			}
			return nil
		}
	}
	return errors.New("invalid model response")
}

func DecodeJSON(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func ValidID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func validArtifact(value string) bool {
	const prefix = "sha256-"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

// ValidModelKey 限定公开模型键。业务含义变化时使用新键，避免旧登记接到不同业务的请求。
func ValidModelKey(model string) bool {
	switch model {
	case WardrobeModel, ClothGenModel, OutfitModel, BodyModel, VideoModel:
		return true
	default:
		return false
	}
}

type InputImage struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// 受理和原生派发使用同一种图片格式，避免已有队列记录被不同的规则拒绝。
func DecodeInput(input json.RawMessage) (map[string]json.RawMessage, []InputImage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil || fields == nil {
		return nil, nil, errors.New("input must be a JSON object")
	}
	var images []InputImage
	if err := DecodeJSON(fields["images"], &images); err != nil {
		return nil, nil, errors.New("images must contain mime_type and base64 data")
	}
	for _, image := range images {
		if !imageMIME.MatchString(image.MIMEType) || len(image.Data) == 0 {
			return nil, nil, errors.New("invalid image")
		}
	}
	delete(fields, "images")
	return fields, images, nil
}

// ValidateInput 按模型键校验业务字段。缺少拆衣输入时不能转成商品图请求。
func ValidateInput(model string, input json.RawMessage) error {
	if !ValidModelKey(model) {
		return errors.New("unknown model")
	}
	fields, images, err := DecodeInput(input)
	if err != nil {
		return err
	}
	minImages, maxImages := 1, 1
	var allowed []string
	switch model {
	case ClothGenModel:
		allowed = []string{"garment_name", "garment_role"}
	case WardrobeModel:
		if len(images) == 1 && fields["bbox"] == nil && fields["parse_labels"] == nil {
			return errors.New("garment extraction requires bbox or parser input")
		}
		maxImages = 2
		allowed = []string{"garment_name", "garment_role", "bbox", "bbox_format", "parse_labels"}
	case OutfitModel:
		minImages, maxImages = 2, 2
		allowed = []string{"prompt"}
	case BodyModel:
		allowed = []string{"gender"}
	case VideoModel:
		allowed = []string{"prompt", "motion_tips"}
	}
	if len(images) < minImages || len(images) > maxImages {
		return errors.New("wrong number of input images")
	}
	for key, raw := range fields {
		if !slices.Contains(allowed, key) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("unsupported business field")
		}
		var target any
		switch key {
		case "bbox":
			var coordinates []float64
			if json.Unmarshal(raw, &coordinates) != nil || len(coordinates) != 4 {
				return errors.New("bbox must contain four coordinates")
			}
			continue
		case "parse_labels":
			target = new(map[string]string)
		default:
			target = new(string)
		}
		if json.Unmarshal(raw, target) != nil {
			return errors.New("invalid business field")
		}
	}
	return nil
}
