package clientconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/CyL-Cly/CodexRM-Server/release"
)

const DefaultRelayServer = "https://codexrm.ooth.tech"

// UpdateFeed is separate from pairing. Local operator configuration overrides the release default.
func UpdateFeed(linux bool) (base, manifest string, err error) {
	raw := release.UpdateEndpoint("CODEXRM_AGENT_UPDATE_SERVER", "")
	if raw == "" {
		return "", "", nil
	}
	base, err = ServerURL(raw)
	if err != nil || !strings.HasPrefix(base, "https://") {
		return "", "", errors.New("CODEXRM_AGENT_UPDATE_SERVER must be an HTTPS base URL (without credentials, query or fragment)")
	}
	name := "update.json"
	if linux {
		name = "linux-update.json"
	}
	return base, base + "/" + name, nil
}

// Connection retains the field names used by older installations.
type Connection struct {
	AllowFileManagement bool   `json:"allowFileManagement,omitempty"`
	Server              string `json:"server"`
	EnrollmentKey       string `json:"enrollmentKey,omitempty"`
	Workspace           string `json:"workspace,omitempty"`
	CodexPath           string `json:"codexPath,omitempty"`
	AutoUpdate          bool   `json:"autoUpdate"`
}

func Load(path string) (Connection, error) {
	c := Connection{AutoUpdate: true}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c.Server = DefaultRelayServer
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), &c)
	return c, err
}

func ServerURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\\\r\n") {
		return "", errors.New("请输入完整的 HTTP(S) 服务器地址，不支持账号、查询参数或片段")
	}
	return u.String(), nil
}

func (c *Connection) Normalize() error {
	var err error
	if strings.TrimSpace(c.Server) != "" {
		if c.Server, err = ServerURL(c.Server); err != nil {
			return fmt.Errorf("远程服务器：%w", err)
		}
	} else {
		c.Server = ""
	}
	c.EnrollmentKey = strings.TrimSpace(c.EnrollmentKey)
	return nil
}

func Save(path string, c Connection) error {
	if err := c.Normalize(); err != nil {
		return err
	}
	return WriteJSON(path, c)
}

func WriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
