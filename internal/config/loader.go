package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 引导文件保存对应地区的连接信息，配置中心由部署地区决定。
type Bootstrap struct {
	NacosURL     string `json:"nacos_url,omitempty"`
	AppConfigURL string `json:"appconfig_url,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
}

type Loader struct {
	Path    string
	nacos   bool
	client  *http.Client
	login   Bootstrap
	token   string
	expires time.Time
}

func NewLoader(path string) (*Loader, error) {
	// 地区在启动时确定，避免热加载引导文件时切到另一个配置中心。
	region := strings.ToUpper(strings.TrimSpace(os.Getenv("XX_WG_REGION")))
	switch region {
	case "CN", "SG", "US":
	default:
		return nil, errors.New("XX_WG_REGION must be CN, SG, or US")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Loader{
		Path:  path,
		nacos: region == "CN",
		client: &http.Client{
			Timeout:       5 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (l *Loader) Close() { l.client.CloseIdleConnections() }

func (l *Loader) Load(ctx context.Context) ([]byte, error) {
	raw, err := os.ReadFile(l.Path)
	if err != nil {
		return nil, errors.New("cannot read configuration bootstrap")
	}
	var bootstrap Bootstrap
	if err = json.Unmarshal(raw, &bootstrap); err != nil {
		return nil, errors.New("invalid configuration bootstrap JSON")
	}
	address := bootstrap.AppConfigURL
	if l.nacos {
		address = bootstrap.NacosURL
	}
	if address == "" {
		return nil, errors.New("missing configuration URL for XX_WG_REGION")
	}
	endpoint, err := url.Parse(address)
	if err != nil {
		return nil, errors.New("invalid configuration URL")
	}
	if l.nacos && bootstrap.Username != "" {
		if l.login != bootstrap || !time.Now().Before(l.expires) {
			loginURL := *endpoint
			loginURL.Path = strings.TrimSuffix(endpoint.Path, "/v1/cs/configs") + "/v1/auth/login"
			loginURL.RawQuery = ""
			body := url.Values{"username": {bootstrap.Username}, "password": {bootstrap.Password}}
			data, err := l.fetch(ctx, http.MethodPost, loginURL.String(), body.Encode())
			if err != nil {
				return nil, err
			}
			var login struct {
				Token string `json:"accessToken"`
				TTL   int    `json:"tokenTtl"`
			}
			if json.Unmarshal(data, &login) != nil || login.Token == "" {
				return nil, errors.New("invalid Nacos login response")
			}
			l.login, l.token = bootstrap, login.Token
			l.expires = time.Now().Add(time.Duration(login.TTL) * time.Second * 9 / 10)
		}
		query := endpoint.Query()
		query.Set("accessToken", l.token)
		endpoint.RawQuery = query.Encode()
	}
	data, err := l.fetch(ctx, http.MethodGet, endpoint.String(), "")
	if err != nil {
		// 服务端提前撤销令牌时，下一次读取重新登录，继续保留已应用的配置。
		l.expires = time.Time{}
	}
	return data, err
}

func (l *Loader) fetch(ctx context.Context, method, endpoint, body string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, errors.New("cannot create configuration request")
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := l.client.Do(request)
	if err != nil {
		// url.Error 会带出 Nacos accessToken，日志只报告阶段，不回传原始 URL。
		return nil, errors.New("configuration center unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("configuration center HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("cannot read configuration response")
	}
	return data, nil
}
