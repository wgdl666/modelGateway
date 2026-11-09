package config

import (
	"errors"
	"strings"
)

type NativeBackend struct {
	TargetInstance string `json:"target_instance,omitempty"`
	VTONEdits      bool   `json:"vton_edits,omitempty"`
}

func (b NativeBackend) Matches(capability string) bool {
	return (b.TargetInstance != "") == (capability == "video")
}

// 探针与执行使用同一地址，防止登记其他服务的探针时把原生凭据传过去。
func (b NativeBackend) Accepts(capability, endpoint, healthURL string) bool {
	return b.Matches(capability) && (healthURL == endpoint || strings.HasPrefix(healthURL, endpoint+"/"))
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
		if strings.TrimSpace(backend.TargetInstance) != backend.TargetInstance {
			return errors.New("invalid native_backends target instance")
		}
		if backend.VTONEdits && backend.TargetInstance != "" {
			return errors.New("native_backends vton_edits requires an image endpoint")
		}
	}
	return nil
}
