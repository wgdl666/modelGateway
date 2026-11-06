package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrBusy = errors.New("no available capacity")
var ErrConflict = errors.New("instance identity or capacity conflict")

// 注册身份不可原地换模型或地址；新进程使用新 ID，保留旧请求的后端归属。
type Instance struct {
	ID         string `json:"instance_id"`
	ModelKey   string `json:"model_key"`
	Endpoint   string `json:"endpoint"`
	MachineID  string `json:"machine_id"`
	GPUDevices []int  `json:"gpu_devices"`
	Capacity   int    `json:"capacity_units"`
	Ready      bool   `json:"ready"`
}

type Submission struct {
	ID       string          `json:"request_id"`
	ModelKey string          `json:"model_key"`
	Units    int             `json:"units"`
	Input    json.RawMessage `json:"input"`
}

// 原始业务 JSON 独立保存；同一 ID 重试只读旧记录，不覆盖参数、不重复占用。
type Request struct {
	ID            string          `json:"request_id"`
	ModelKey      string          `json:"model_key"`
	Payload       json.RawMessage `json:"request_payload"`
	InstanceID    string          `json:"instance_id"`
	BackendTaskID *string         `json:"backend_task_id,omitempty"`
	Units         int             `json:"reserved_units"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type Object struct {
	Provider string `json:"provider"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
}

type BackendResponse struct {
	Status string          `json:"status"`
	TaskID *string         `json:"backend_task_id,omitempty"`
	Result json.RawMessage `json:"result"`
}

func (r BackendResponse) validate() error {
	switch r.Status {
	case "running":
		return nil
	case "failed":
		var e struct {
			Code    string `json:"error_code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(r.Result, &e) != nil || e.Code == "" {
			return errors.New("failed response requires error_code")
		}
		return nil
	case "succeeded":
		var result struct {
			Files []Object `json:"files"`
		}
		decoder := json.NewDecoder(bytes.NewReader(r.Result))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil || len(result.Files) == 0 {
			return errors.New("successful response requires object files")
		}
		for _, f := range result.Files {
			if (f.Provider != "s3" && f.Provider != "oss") || f.Region == "" || f.Bucket == "" || f.Key == "" {
				return errors.New("invalid object reference")
			}
		}
		return nil
	default:
		return errors.New("invalid backend status")
	}
}

func validID(id string) bool { parsed, e := uuid.Parse(id); return e == nil && parsed.String() == id }

// 只允许运维指定网段中的字面 IP，避免注册接口被用来访问任意地址或 DNS 重绑定。
func validateEndpoint(endpoint string, networks []*net.IPNet, ports map[string]bool) error {
	u, e := url.Parse(endpoint)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return errors.New("invalid endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return errors.New("endpoint must use an allowed literal IP")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if !ports[port] {
		return errors.New("endpoint port not allowed")
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return nil
		}
	}
	return errors.New("endpoint network not allowed")
}
