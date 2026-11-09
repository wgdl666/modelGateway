package config

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var gpuEndpointPath = regexp.MustCompile(`^/gpu(0|[1-9][0-9]*)$`)

type NativeBackend struct {
	// 地址固定使用图片或视频协议，避免通过实例登记改变该地址可用的原生凭据。
	Video     bool `json:"video,omitempty"`
	VTONEdits bool `json:"vton_edits,omitempty"`
}

func (b NativeBackend) Matches(capability string) bool {
	return b.Video == (capability == "video")
}

func isGPUEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && gpuEndpointPath.MatchString(u.Path)
}

// 单卡地址由运维配置授权，探针必须落在该卡，避免把另一张卡的就绪状态用于派发。
func (c Config) AcceptsInstanceAddress(capability, endpoint, healthURL, targetInstance string) bool {
	if backend, direct := c.NativeBackends[endpoint]; direct {
		return backend.Accepts(capability, endpoint, healthURL, targetInstance)
	}
	return targetInstance == "" && !isGPUEndpoint(endpoint)
}

func (b NativeBackend) Accepts(capability, endpoint, healthURL, targetInstance string) bool {
	if !b.Matches(capability) || strings.TrimSpace(targetInstance) != targetInstance {
		return false
	}
	if isGPUEndpoint(endpoint) {
		return b.Video && targetInstance == "" && healthURL == endpoint+"/healthz"
	}
	// 路演现有根地址在排空前仍靠旧实例目标派发，保证原来已登记的请求可以回查。
	return b.Video == (targetInstance != "") &&
		(healthURL == endpoint || strings.HasPrefix(healthURL, endpoint+"/"))
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
		if isGPUEndpoint(endpoint) && !backend.Video {
			return errors.New("native_backends /gpuN requires a video backend")
		}
		if backend.VTONEdits && backend.Video {
			return errors.New("native_backends vton_edits requires an image endpoint")
		}
	}
	return nil
}
