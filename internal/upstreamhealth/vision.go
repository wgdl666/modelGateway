package upstreamhealth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type VisionService struct {
	Ready  *bool           `json:"ready"`
	Health json.RawMessage `json:"health"`
}

// ReadVision 接受聚合 503 的细项。一张卡异常时，仍保留其他实际副本的探测结果。
func ReadVision(ctx context.Context, client *http.Client, healthURL string) map[string]VisionService {
	// G6 分批探测，异常时一轮最多约 20 秒。
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil
	}
	var health struct {
		Services map[string]VisionService `json:"services"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&health); err != nil {
		return nil
	}
	return health.Services
}
