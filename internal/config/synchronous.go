package config

import (
	"strconv"
	"strings"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

// SynchronousHealthURL 只探测部署配置中的固定入口，登记表里的本机端口用于识别副本。
func (c Config) SynchronousHealthURL(kind string) string {
	switch protocol.SynchronousKind(kind) {
	case protocol.SyncVision:
		if c.VisionAddr != "" {
			return "http://" + c.VisionAddr + "/health"
		}
	case protocol.SyncDream:
		if c.VLMAddr != "" {
			return "http://" + c.VLMAddr + "/health"
		}
	case protocol.SyncDecision:
		if c.DecisionAddr != "" {
			return "http://" + c.DecisionAddr + "/v1/status"
		}
	}
	return ""
}

func (c Config) acceptsSynchronousTarget(kind, endpoint, healthURL string) bool {
	if want := c.SynchronousHealthURL(kind); want == "" || healthURL != want {
		return false
	}
	if kind == protocol.ChatProtocol {
		return endpoint == "http://"+c.VLMAddr
	}
	port, local := strings.CutPrefix(endpoint, "http://127.0.0.1:")
	n, err := strconv.Atoi(port)
	return local && err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == port
}
