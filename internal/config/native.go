package config

import (
	"errors"
	"strings"
)

type NativeBackend struct {
	// 地址固定使用图片或视频协议，避免通过实例登记改变该地址可用的原生凭据。
	Video     bool `json:"video,omitempty"`
	VTONEdits bool `json:"vton_edits,omitempty"`
}

func (b NativeBackend) Matches(capability string) bool {
	return b.Video == (capability == "video")
}

// 原生视频必须登记目标。探针与执行使用同一地址，避免向其他服务发送原生凭据。
func (b NativeBackend) Accepts(capability, endpoint, healthURL, targetInstance string) bool {
	if !b.Matches(capability) || b.Video != (targetInstance != "") {
		return false
	}
	return strings.TrimSpace(targetInstance) == targetInstance &&
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
		if backend.VTONEdits && backend.Video {
			return errors.New("native_backends vton_edits requires an image endpoint")
		}
	}
	return nil
}
