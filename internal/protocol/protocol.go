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
	MaxCapacityUnits = 65536

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
// 原生视频用地址中的单卡路径绑定 GPU。
type Instance struct {
	ID       string `json:"instance_id"`
	ModelKey string `json:"model_key"`
	// 协议由登记显式指定，模型名称只作为身份与路由标识。
	Protocol      string `json:"protocol"`
	Endpoint      string `json:"endpoint"`
	HealthURL     string `json:"health_url"`
	Artifact      string `json:"artifact_digest"`
	ResourceGroup string `json:"resource_group"`
	// 现有部署程序可能仍传旧字段，只有资源组管理接口可以设置组上限。
	GroupCapacity int    `json:"group_capacity_units,omitempty"`
	Capacity      int    `json:"capacity_units"`
	MachineID     string `json:"machine_id,omitempty"`
	GPUDevices    []int  `json:"gpu_devices,omitempty"`
}

type InstanceTarget struct {
	ModelKey   string `json:"model_key"`
	Protocol   string `json:"protocol"`
	Endpoint   string `json:"endpoint"`
	HealthURL  string `json:"health_url"`
	GPUDevices []int  `json:"gpu_devices,omitempty"`
}

type InstanceHealth struct {
	Ready          bool   `json:"ready"`
	ArtifactDigest string `json:"artifact_digest,omitempty"`
}

// Valid 检查部署身份和容量。地址还需按当前配置的出站白名单验证。
func (i Instance) Valid() bool {
	// 服务未上报摘要时留空，避免把未知部署记成某个真实版本。
	return ValidInstanceID(i.ID) && ValidModelKey(i.ModelKey) && ValidProtocol(i.Protocol) &&
		ValidResourceGroupID(i.ResourceGroup) && (i.Artifact == "" || ValidArtifact(i.Artifact)) &&
		i.Capacity >= 1 && i.Capacity <= MaxCapacityUnits
}

func ValidResourceGroupID(id string) bool {
	// 登记和更新使用数据库中的原组标识，迁移不会重命名历史组。
	return id != "" && len(id) <= 256
}

func ValidInstanceID(id string) bool {
	return instanceID.MatchString(id)
}

type Metadata struct {
	UserID  string `json:"user_id,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
}

type Submission struct {
	Input json.RawMessage `json:"input"`
	// 输出能力来自调用方，具体协议只能来自登记；旧调用方缺省时沿用原入口契约。
	Output   string   `json:"output,omitempty"`
	Metadata Metadata `json:"metadata,omitempty"`
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
	Generation *Generation `json:"generation,omitempty"`
	Files      []Object    `json:"files,omitempty"`
	HTTP       *HTTPResult `json:"http,omitempty"`
}

// HTTPResult 只记录同步响应的执行证据，原始文本、图片和流仍按现有协议交给调用方。
type HTTPResult struct {
	StatusCode    int    `json:"status_code"`
	ContentType   string `json:"content_type"`
	ResponseBytes int64  `json:"response_bytes"`
}

type RequestError struct {
	Retries *GenerationRetries `json:"retries,omitempty"`
	Code    string             `json:"code"`
	Message string             `json:"message"`
}

type Response struct {
	TraceID   string        `json:"trace_id,omitempty"`
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

func ValidArtifact(value string) bool {
	const prefix = "sha256-"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

// ValidModelKey 只校验标识格式，是否支持由实例登记的协议决定。
func ValidModelKey(model string) bool {
	return instanceID.MatchString(model)
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

// 实验凭据在业务字段之外可覆盖的生成参数，原样转发给模型服务。
// 生产凭据不能覆盖服务端默认的文案、提示词增强和视频规格。
var experimentalFields = map[string][]string{
	WardrobeProtocol: {"prompt", "use_pe"},
	BodyProtocol:     {"prompt"},
	OutfitProtocol:   {"use_pe"},
	VideoProtocol:    {"model", "duration", "resolution", "fps", "audio"},
}

// ValidateInput 按登记协议校验业务字段。缺少拆衣输入时不能转成商品图请求。
func ValidateInput(kind string, input json.RawMessage) error {
	return validateInput(kind, input, false)
}

// ValidateExperimentalInput 在业务字段之外放行该协议的实验参数，仅用于实验凭据。
func ValidateExperimentalInput(kind string, input json.RawMessage) error {
	return validateInput(kind, input, true)
}

func validateInput(kind string, input json.RawMessage, experimental bool) error {
	if GenerationProtocol(kind) {
		return ValidateGenerationInput(kind, input)
	}
	if !MediaProtocol(kind) {
		return errors.New("unsupported media protocol")
	}
	fields, images, err := DecodeInput(input)
	if err != nil {
		return err
	}
	minImages, maxImages := 1, 1
	var allowed []string
	switch kind {
	case ClothGenProtocol:
		allowed = []string{"garment_name", "garment_role"}
	case WardrobeProtocol:
		if len(images) == 1 && fields["bbox"] == nil && fields["parse_labels"] == nil {
			return errors.New("garment extraction requires bbox or parser input")
		}
		maxImages = 2
		allowed = []string{"garment_name", "garment_role", "bbox", "bbox_format", "parse_labels"}
	case OutfitProtocol:
		minImages, maxImages = 2, 2
		allowed = []string{"prompt"}
	case BodyProtocol:
		allowed = []string{"gender"}
	case VideoProtocol:
		allowed = []string{"prompt", "motion_tips"}
	}
	if experimental {
		allowed = append(allowed, experimentalFields[kind]...)
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
