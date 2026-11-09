package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

const (
	appConfigPollInterval = 45 * time.Second
	maxConfigBytes        = 1 << 20
)

// 引导文件保存对应地区的连接信息，配置中心由部署地区决定。
type Bootstrap struct {
	NacosURL     string `json:"nacos_url,omitempty"`
	AppConfigURL string `json:"appconfig_url,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
}

type Loader struct {
	nacos        nacosConfigClient
	nacosKey     vo.ConfigParam
	appConfigURL string
	client       *http.Client
	pollInterval time.Duration
	applied      []byte
}

func NewLoader(path string) (*Loader, error) {
	region := strings.ToUpper(strings.TrimSpace(os.Getenv("XX_WG_REGION")))
	switch region {
	case "CN", "SG", "US":
	default:
		return nil, errors.New("XX_WG_REGION must be CN, SG, or US")
	}
	// 配置中心的定位和连接在启动时固定，热更新只读取业务配置正文。
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read configuration bootstrap")
	}
	var bootstrap Bootstrap
	if err := json.Unmarshal(raw, &bootstrap); err != nil {
		return nil, errors.New("invalid configuration bootstrap JSON")
	}
	if region == "CN" {
		return newNacosLoader(bootstrap)
	}
	if bootstrap.AppConfigURL == "" {
		return nil, errors.New("missing configuration URL for XX_WG_REGION")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Loader{
		appConfigURL: bootstrap.AppConfigURL,
		pollInterval: appConfigPollInterval,
		client: &http.Client{
			Timeout:       5 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (l *Loader) Close() {
	if l.nacos != nil {
		l.nacos.CloseClient()
		return
	}
	l.client.CloseIdleConnections()
}

func (l *Loader) Load(ctx context.Context) (*Runtime, error) {
	raw, err := l.read(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	runtime, err := NewRuntime(cfg)
	if err != nil {
		return nil, err
	}
	l.applied = raw
	return runtime, nil
}

// Listen 串行应用配置。stop 等待最后一次应用结束，之后 HTTP 服务才可关闭监听。
func (l *Loader) Listen(ctx context.Context, runtime *Runtime, replaceListener func(net.Listener)) (func(), error) {
	changes := make(chan struct{}, 1)
	if l.nacos != nil {
		key := l.nacosKey
		key.OnChange = func(_, _, _, _ string) {
			// SDK 并发调用回调，合并通知后重新读当前版本，避免迟到的回调覆盖新配置。
			select {
			case changes <- struct{}{}:
			default:
			}
		}
		if err := l.nacos.ListenConfig(key); err != nil {
			return nil, errors.New("cannot listen for Nacos configuration changes")
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ticks <-chan time.Time
		if l.nacos == nil {
			ticker := time.NewTicker(l.pollInterval)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-changes:
			case <-ticks:
			}
			l.refresh(ctx, runtime, replaceListener)
		}
	}()
	return func() {
		cancel()
		<-done
		if l.nacos != nil {
			_ = l.nacos.CancelListenConfig(l.nacosKey)
		}
	}, nil
}

func (l *Loader) refresh(ctx context.Context, runtime *Runtime, replaceListener func(net.Listener)) {
	raw, err := l.read(ctx)
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		if bytes.Equal(l.applied, raw) {
			return
		}
		err = applyConfig(raw, runtime, replaceListener)
	}
	if err != nil {
		slog.Error("configuration reload failed", "reason", err.Error())
		return
	}
	l.applied = raw
	slog.Info("configuration reloaded")
}

func applyConfig(raw []byte, runtime *Runtime, replaceListener func(net.Listener)) error {
	next, err := Parse(raw)
	if err != nil {
		return err
	}
	var listener net.Listener
	if next.Listen != runtime.Current().Listen {
		// 新地址须先绑定成功，避免配置已生效却无法接入新地址。
		listener, err = net.Listen("tcp", next.Listen)
		if err != nil {
			return errors.New("cannot listen on configured address")
		}
	}
	if err := runtime.Reload(next); err != nil {
		if listener != nil {
			_ = listener.Close()
		}
		return err
	}
	if listener != nil {
		replaceListener(listener)
	}
	return nil
}

func (l *Loader) read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.nacos != nil {
		data, err := l.nacos.GetConfig(l.nacosKey)
		if err != nil || len(data) > maxConfigBytes {
			return nil, errors.New("cannot read Nacos configuration")
		}
		return []byte(data), nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.appConfigURL, nil)
	if err != nil {
		return nil, errors.New("cannot create configuration request")
	}
	response, err := l.client.Do(request)
	if err != nil {
		// 配置错误只记录失败阶段，避免暴露引导地址和凭据。
		return nil, errors.New("AppConfig Agent unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("configuration center HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return nil, errors.New("cannot read configuration response")
	}
	return data, nil
}
