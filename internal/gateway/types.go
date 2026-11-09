package gateway

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	wardrobeModel = "Vmind_wardrobe_v1.0"
	outfitModel   = "Vmind_outfit_v1.0"
	bodyModel     = "Vmind_bodymodel_v1.0"
	videoModel    = "Vmind_outfit_video_v1.0"
	probeInterval = 2 * time.Second
	probeMaxAge   = 9 * time.Second
)

var (
	ErrBusy     = errors.New("no healthy model instance")
	ErrFull     = errors.New("waiting queue is full")
	ErrConflict = errors.New("request or instance conflict")
	imageMIME   = regexp.MustCompile(`^image/[A-Za-z0-9.+-]+$`)
	instanceID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,255}$`)
)

// Instance 记录部署身份和同卡分组。是否可派由主动探针及排空状态决定。
type Instance struct {
	ID            string `json:"instance_id"`
	ModelKey      string `json:"model_key"`
	Capability    string `json:"capability"`
	Endpoint      string `json:"endpoint"`
	HealthURL     string `json:"health_url"`
	Artifact      string `json:"artifact_digest"`
	ResourceGroup string `json:"resource_group"`
	GroupCapacity int    `json:"group_capacity_units"`
	Capacity      int    `json:"capacity_units"`
	MachineID     string `json:"machine_id,omitempty"`
	GPUDevices    []int  `json:"gpu_devices,omitempty"`
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
	RequestID  string `json:"request_id"`
	ModelKey   string `json:"model_key"`
	Capability string `json:"capability"`
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

func (r ModelResponse) validate(id string) error {
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

func decodeJSON(raw []byte, out any) error {
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

func validID(id string) bool {
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

func capabilities(model string) []string {
	switch model {
	case wardrobeModel:
		return []string{"cloth_gen", "garment_extraction"}
	case outfitModel:
		return []string{"vton"}
	case bodyModel:
		return []string{"body_model"}
	case videoModel:
		return []string{"video"}
	default:
		return nil
	}
}

// 模型标识和业务输入决定能力，客户端不能提交模型地址、内部路径或执行容量。
func route(model string, input json.RawMessage) (string, error) {
	if len(capabilities(model)) == 0 {
		return "", errors.New("unknown model")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil || fields == nil {
		return "", errors.New("input must be a JSON object")
	}
	var images []struct {
		MIMEType string `json:"mime_type"`
		Data     []byte `json:"data"`
	}
	if err := decodeJSON(fields["images"], &images); err != nil {
		return "", errors.New("images must contain mime_type and base64 data")
	}
	for _, image := range images {
		if !imageMIME.MatchString(image.MIMEType) || len(image.Data) == 0 {
			return "", errors.New("invalid image")
		}
	}
	delete(fields, "images")
	capability := capabilities(model)[0]
	minImages, maxImages := 1, 1
	var allowed []string
	switch model {
	case wardrobeModel:
		allowed = []string{"garment_name", "garment_role"}
		if fields["bbox"] != nil || fields["parse_labels"] != nil || len(images) > 1 {
			capability = "garment_extraction"
			maxImages = 2
			allowed = append(allowed, "bbox", "bbox_format", "parse_labels")
		}
	case outfitModel:
		minImages, maxImages = 2, 2
		allowed = []string{"prompt"}
	case bodyModel:
		allowed = []string{"gender"}
	case videoModel:
		allowed = []string{"prompt", "motion_tips"}
	}
	if len(images) < minImages || len(images) > maxImages {
		return "", errors.New("wrong number of input images")
	}
	for key, raw := range fields {
		if !slices.Contains(allowed, key) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return "", errors.New("unsupported business field")
		}
		var target any
		switch key {
		case "bbox":
			var coordinates []float64
			if json.Unmarshal(raw, &coordinates) != nil || len(coordinates) != 4 {
				return "", errors.New("bbox must contain four coordinates")
			}
			continue
		case "parse_labels":
			target = new(map[string]string)
		default:
			target = new(string)
		}
		if json.Unmarshal(raw, target) != nil {
			return "", errors.New("invalid business field")
		}
	}
	return capability, nil
}

// 注册和实际出站都限制到允许的字面 IP 与端口，健康地址可带明确路径。
func validateEndpoint(endpoint string, health bool, networks []*net.IPNet, ports map[string]bool) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || (!health && strings.Trim(u.Path, "/") != "") {
		return errors.New("invalid endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	port := u.Port()
	if port == "" && u.Scheme == "https" {
		port = "443"
	} else if port == "" {
		port = "80"
	}
	if ip == nil || !ports[port] {
		return errors.New("endpoint is outside the backend allowlist")
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return nil
		}
	}
	return errors.New("endpoint is outside the backend allowlist")
}
