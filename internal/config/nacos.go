package config

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

type nacosConfigClient interface {
	GetConfig(vo.ConfigParam) (string, error)
	ListenConfig(vo.ConfigParam) error
	CancelListenConfig(vo.ConfigParam) error
	CloseClient()
}

func newNacosLoader(bootstrap Bootstrap) (*Loader, error) {
	// 沿用部署已有的配置 URL，从中提取 SDK 所需的服务地址和文档坐标。
	const configPath = "/v1/cs/configs"
	address, err := url.Parse(bootstrap.NacosURL)
	if err != nil || address.Hostname() == "" || !strings.HasSuffix(address.Path, configPath) ||
		(address.Scheme != "http" && address.Scheme != "https") {
		return nil, errors.New("invalid Nacos configuration URL")
	}
	port := uint64(80)
	if address.Scheme == "https" {
		port = 443
	}
	if text := address.Port(); text != "" {
		port, err = strconv.ParseUint(text, 10, 16)
		if err != nil || port == 0 {
			return nil, errors.New("invalid Nacos server port")
		}
	}
	query, err := url.ParseQuery(address.RawQuery)
	if err != nil || query.Get("dataId") == "" {
		return nil, errors.New("invalid Nacos configuration coordinates")
	}
	group := query.Get("group")
	if group == "" {
		group = constant.DEFAULT_GROUP
	}

	dir := filepath.Join(os.TempDir(), "model-gateway", "nacos")
	// 只使用本次从配置中心读到的正文，避免启动时回退到 SDK 磁盘快照。
	clientConfig := constant.NewClientConfig(
		constant.WithTimeoutMs(5000),
		constant.WithNamespaceId(query.Get("tenant")),
		constant.WithUsername(bootstrap.Username),
		constant.WithPassword(bootstrap.Password),
		constant.WithDisableUseSnapShot(true),
		constant.WithNotLoadCacheAtStart(true),
		constant.WithLogLevel("warn"),
		constant.WithLogDir(filepath.Join(dir, "logs")),
		constant.WithCacheDir(filepath.Join(dir, "cache")),
	)
	server := constant.NewServerConfig(address.Hostname(), port,
		constant.WithScheme(address.Scheme),
		constant.WithContextPath(strings.TrimSuffix(address.Path, configPath)),
	)
	client, err := clients.NewConfigClient(vo.NacosClientParam{
		ClientConfig:  clientConfig,
		ServerConfigs: []constant.ServerConfig{*server},
	})
	if err != nil {
		return nil, errors.New("cannot initialize Nacos configuration client")
	}
	return &Loader{nacos: client, nacosKey: vo.ConfigParam{DataId: query.Get("dataId"), Group: group}}, nil
}
