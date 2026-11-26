package agenthost

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type wxComposeService struct {
	key, name string
	// Optional services are included only when explicitly selected.
	optionalYAML string
}

var wxComposeServices = []wxComposeService{
	{key: "redis", name: "wechat-admin-redis"},
	{key: "mysql", name: "wechat-admin-mysql"},
	{key: "qdrant", name: "wechat-admin-qdrant"},
	{key: "wechat-auth", name: "wechat-server"},
	{key: "slider", name: "wechat-slider"},
	{key: "uuid", name: "wechat-uuid"},
	{key: "word-cloud", name: "word-cloud-server", optionalYAML: `  word-cloud-server:
    image: @@word-cloud@@
    container_name: word-cloud-server
    restart: always
    networks: [wechat-robot]
`},
	{key: "jimeng", name: "jimeng-api", optionalYAML: `  jimeng-api:
    image: @@jimeng@@
    container_name: jimeng-api
    restart: always
    networks: [wechat-robot]
    environment:
      TZ: Asia/Shanghai
`},
	{key: "mcp", name: "wechat-robot-mcp-server", optionalYAML: `  wechat-robot-mcp-server:
    image: @@mcp@@
    container_name: wechat-robot-mcp-server
    restart: always
    networks: [wechat-robot]
    environment:
      MCP_SERVER_PORT: 9000
      GIN_MODE: release
      TZ: Asia/Shanghai
      MYSQL_HOST: wechat-admin-mysql
      MYSQL_PORT: 3306
      MYSQL_USER: root
      MYSQL_PASSWORD: ${MYSQL_ROOT_PASSWORD}
`},
	{key: "admin-web", name: "wechat-robot-admin-frontend"},
	{key: "admin-api", name: "wechat-robot-admin-backend"},
	{key: "nginx", name: "wechat-nginx"},
}

var wxImageKeys = func() []string {
	keys := make([]string, 0, len(wxComposeServices)+3)
	for _, service := range wxComposeServices {
		keys = append(keys, service.key)
	}
	return append(keys, "client", "protocol", "backup")
}()

func validateWXOptionalServices(keys []string) error {
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		optional := slices.ContainsFunc(wxComposeServices, func(service wxComposeService) bool {
			return service.key == key && service.optionalYAML != ""
		})
		if !optional || seen[key] {
			return errors.New("可选服务选择无效")
		}
		seen[key] = true
	}
	return nil
}

func wxEnabledComposeServices(keys []string) []wxComposeService {
	services := make([]wxComposeService, 0, len(wxComposeServices))
	for _, service := range wxComposeServices {
		if service.optionalYAML == "" || slices.Contains(keys, service.key) {
			services = append(services, service)
		}
	}
	return services
}

// Look at the existing managed Compose file so upgrading preserves selections,
// including services that were always present in older installations.
func wxExistingOptionalServices(compose string) []string {
	var keys []string
	for _, service := range wxComposeServices {
		if service.optionalYAML != "" && strings.Contains(compose, "\n  "+service.name+":\n") {
			keys = append(keys, service.key)
		}
	}
	return keys
}

var wxImageRef = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]+(?::[0-9]+)?/[a-z0-9][a-z0-9_./-]*@sha256:[a-f0-9]{64}$`)

// Populated by the Linux release build so binary-only updates carry the matching lock.
var wxReleaseImageLock string

func wxImageLock() (map[string]string, error) {
	path := os.Getenv("CODEXRM_WXBOT_IMAGE_LOCK")
	if path == "" && wxReleaseImageLock != "" {
		raw, err := base64.StdEncoding.DecodeString(wxReleaseImageLock)
		if err != nil {
			return nil, fmt.Errorf("读取内置 wxbot 镜像清单: %w", err)
		}
		var images map[string]string
		if err := json.Unmarshal(raw, &images); err != nil {
			return nil, fmt.Errorf("解析内置 wxbot 镜像清单: %w", err)
		}
		return images, validateWXImageLock(images)
	}
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(filepath.Dir(exe), "wx-images.json")
	}
	images, err := readJSONFile[map[string]string](path)
	if err != nil {
		return nil, fmt.Errorf("读取 wxbot 镜像清单: %w", err)
	}
	if images == nil {
		return nil, errors.New("当前安装包缺少 wx-images.json 镜像清单")
	}
	return *images, validateWXImageLock(*images)
}

func validateWXImageLock(images map[string]string) error {
	for _, key := range wxImageKeys {
		if !wxImageRef.MatchString(images[key]) || strings.Contains(images[key], "..") {
			return fmt.Errorf("wxbot 镜像 %s 缺少有效的仓库地址或 SHA-256 摘要", key)
		}
	}
	return nil
}

func wxRenderCompose(images map[string]string, optionalServices []string) string {
	var optional strings.Builder
	for _, service := range wxEnabledComposeServices(optionalServices) {
		optional.WriteString(service.optionalYAML)
	}
	template := strings.Replace(wxComposeTemplate, "@@optional-services@@", optional.String(), 1)
	for _, key := range wxImageKeys {
		template = strings.ReplaceAll(template, "@@"+key+"@@", images[key])
	}
	return template
}
