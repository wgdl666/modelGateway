package config

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

var gpuEndpointPath = regexp.MustCompile(`^/gpu(0|[1-9][0-9]*)$`)

type NativeBackend struct {
	// 地址固定使用图片或视频协议，避免通过实例登记改变该地址可用的原生凭据。
	Video     bool `json:"video,omitempty"`
	VTONEdits bool `json:"vton_edits,omitempty"`
}

func (b NativeBackend) Matches(model string) bool {
	return b.Video == (model == protocol.VideoModel)
}

func isGPUEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && gpuEndpointPath.MatchString(u.Path)
}

// 手动探活与实例登记使用同一出站规则，表单地址必须符合所选网关的配置。
func (s *Snapshot) ValidateInstanceTarget(target protocol.InstanceTarget) error {
	if err := s.ValidateEndpoint(target.Endpoint, false); err != nil {
		return err
	}
	if err := s.ValidateEndpoint(target.HealthURL, true); err != nil {
		return err
	}
	if !s.AcceptsInstanceAddress(target.ModelKey, target.Endpoint, target.HealthURL) {
		return errors.New("endpoint, health URL or model_key does not match the backend configuration")
	}
	return nil
}

// 单卡地址使用原生视频协议，探活也须命中同一张卡，避免用其他 worker 的就绪状态派单。
func (c Config) AcceptsInstanceAddress(model, endpoint, healthURL string) bool {
	backend, direct := c.NativeBackends[endpoint]
	gpu := isGPUEndpoint(endpoint)
	if !direct {
		return !gpu
	}
	if !backend.Matches(model) || backend.Video != gpu {
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

// 原生协议由运维配置绑定到原生地址，注册凭据只能选择已允许的执行地址。
func (s *Snapshot) validateNative() error {
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
