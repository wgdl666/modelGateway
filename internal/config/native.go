package config

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

var gpuEndpointPath = regexp.MustCompile(`^/gpu(0|[1-9][0-9]*)$`)
var gpuEndpointSuffix = regexp.MustCompile(`/gpu(0|[1-9][0-9]*)$`)

// A video instance identifies one physical GPU. Named GPU paths must agree;
// endpoints using dedicated ports or other base paths remain valid.
func (c Config) AcceptsInstanceGPU(model, endpoint string, devices []int) bool {
	backend, native := c.NativeBackendFor(model, endpoint)
	if !native || !backend.Video {
		return true
	}
	if len(devices) != 1 || devices[0] < 0 {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	match := gpuEndpointSuffix.FindStringSubmatch(u.Path)
	return match == nil || match[1] == strconv.Itoa(devices[0])
}

type NativeBackend struct {
	// 按模型类型选择原生协议；执行地址来自实例登记。
	Video     bool `json:"video,omitempty"`
	VTONEdits bool `json:"vton_edits,omitempty"`
}

// NativeBackendFor selects the protocol, never an address. Execution always uses
// the endpoint captured from the selected instance. Explicit legacy bindings
// remain supported during migration; new GPU instances need no binding here.
func (c Config) NativeBackendFor(model, endpoint string) (NativeBackend, bool) {
	if backend, ok := c.NativeBackends[endpoint]; ok {
		return backend, true
	}
	backend, ok := c.NativeModels[model]
	return backend, ok
}

func (b NativeBackend) Matches(kind string) bool {
	return b.Video == (kind == protocol.VideoProtocol)
}

func isGPUEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && gpuEndpointPath.MatchString(u.Path)
}

// 手动探活与实例登记使用同一出站规则，表单地址必须符合所选网关的配置。
func (s *Snapshot) ValidateInstanceTarget(target protocol.InstanceTarget) error {
	if protocol.SynchronousKind(target.Protocol) != "" {
		if p, ok := s.SubjectPoolFor(target.ModelKey, target.Protocol, target.Endpoint); ok && target.HealthURL == p.HealthURL {
			if err := s.ValidateEndpoint(target.Endpoint, false); err != nil {
				return err
			}
			return s.ValidateEndpoint(target.HealthURL, true)
		}
		if s.acceptsSynchronousTarget(target.Protocol, target.Endpoint, target.HealthURL) {
			return nil
		}
		return errors.New("synchronous target must match its configured probe and execution address")
	}
	if err := s.ValidateEndpoint(target.Endpoint, false); err != nil {
		return err
	}
	if err := s.ValidateEndpoint(target.HealthURL, true); err != nil {
		return err
	}
	if !s.AcceptsInstanceAddress(target.ModelKey, target.Protocol, target.Endpoint, target.HealthURL) {
		return errors.New("endpoint, health URL or model_key does not match the backend configuration")
	}
	return nil
}

// 单卡地址使用原生视频协议，探活也须命中同一张卡，避免用其他 worker 的就绪状态派单。
func (c Config) AcceptsInstanceAddress(model, kind, endpoint, healthURL string) bool {
	if protocol.SynchronousKind(kind) != "" {
		if p, ok := c.SubjectPoolFor(model, kind, endpoint); ok {
			return p.HealthURL == healthURL
		}
		return c.acceptsSynchronousTarget(kind, endpoint, healthURL)
	}
	backend, direct := c.NativeBackendFor(model, endpoint)
	gpu := isGPUEndpoint(endpoint)
	if !direct {
		u, err := url.Parse(endpoint)
		return err == nil && !gpu && (u.Path == "" || u.Path == "/")
	}
	_, legacy := c.NativeBackends[endpoint]
	if !backend.Matches(kind) || (legacy && backend.Video != gpu) {
		return false
	}
	if backend.Video {
		return healthURL == endpoint+"/healthz"
	}
	// 探针与执行使用同一地址，避免向其他服务发送原生凭据。
	return healthURL == endpoint || strings.HasPrefix(healthURL, endpoint+"/")
}

type NativeResults struct {
	Provider string `json:"provider"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Endpoint string `json:"endpoint,omitempty"`
}

// 协议按模型配置；旧地址绑定保留用于兼容迁移。
func (s *Snapshot) validateNative() error {
	for model, backend := range s.NativeModels {
		// 模型名称不推断协议，实际登记时再核对该模型配置与登记协议的一致性。
		if !protocol.ValidModelKey(model) || (backend.Video && backend.VTONEdits) {
			return errors.New("invalid native_models protocol")
		}
	}
	for endpoint, backend := range s.NativeBackends {
		if strings.HasSuffix(endpoint, "/") || s.ValidateEndpoint(endpoint, false) != nil {
			return errors.New("invalid native_backends endpoint")
		}
		if backend.Video != isGPUEndpoint(endpoint) {
			return errors.New("only native video backends use a /gpuN endpoint")
		}
		if backend.VTONEdits && backend.Video {
			return errors.New("native_backends vton_edits requires an image endpoint")
		}
	}
	return nil
}
