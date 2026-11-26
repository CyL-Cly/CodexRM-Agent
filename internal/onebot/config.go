// Package onebot implements the OneBot 11 message protocol and transports.
package onebot

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	ModeWS        = "ws"
	ModeWSReverse = "ws-reverse"
	ModeHTTP      = "http"

	StateDisabled   = "disabled"
	StateConnecting = "connecting"
	StateConnected  = "connected"
	StateRetrying   = "retrying"

	maxURLBytes    = 2048
	maxListenBytes = 512
	maxTokenBytes  = 1024
)

// Config describes one OneBot endpoint. Listen is intentionally optional during
// validation so applications can apply their own defaults before Run.
type Config struct {
	Enabled     bool   `json:"enabled"`
	Mode        string `json:"mode"`
	URL         string `json:"url"`
	Listen      string `json:"listen"`
	AccessToken string `json:"accessToken"`
	SelfID      int64  `json:"selfId"`
}

// Validate rejects malformed endpoints and values that cannot safely be used in
// HTTP headers. Disabled configurations may omit Mode and URL.
func (c Config) Validate() error {
	if c.SelfID < 0 {
		return fmt.Errorf("onebot: selfId must not be negative")
	}
	if err := validateToken(c.AccessToken); err != nil {
		return err
	}
	if len(c.Listen) > maxListenBytes {
		return fmt.Errorf("onebot: listen exceeds %d bytes", maxListenBytes)
	}
	if strings.TrimSpace(c.Listen) != c.Listen {
		return fmt.Errorf("onebot: listen must not have surrounding whitespace")
	}
	if c.Listen != "" {
		_, port, err := net.SplitHostPort(c.Listen)
		if err != nil {
			return fmt.Errorf("onebot: invalid listen address: %w", err)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("onebot: listen port must be a number from 0 to 65535: %w", err)
		}
	}

	if !c.Enabled && c.Mode == "" && c.URL == "" {
		return nil
	}
	switch c.Mode {
	case ModeWS, ModeWSReverse, ModeHTTP:
	default:
		return fmt.Errorf("onebot: mode must be %q, %q, or %q", ModeWS, ModeWSReverse, ModeHTTP)
	}
	if len(c.URL) > maxURLBytes {
		return fmt.Errorf("onebot: url exceeds %d bytes", maxURLBytes)
	}
	if strings.TrimSpace(c.URL) != c.URL {
		return fmt.Errorf("onebot: url must not have surrounding whitespace")
	}
	if !c.Enabled && c.URL == "" {
		return nil
	}
	if c.Enabled && (c.Mode == ModeWSReverse || c.Mode == ModeHTTP) {
		if err := validateListenerAuth(c.Listen, c.AccessToken); err != nil {
			return err
		}
	}

	switch c.Mode {
	case ModeWS:
		return validateEndpoint(c.URL, "ws", "wss")
	case ModeHTTP:
		return validateEndpoint(c.URL, "http", "https")
	case ModeWSReverse:
		if c.URL != "" {
			return fmt.Errorf("onebot: url is not used in ws-reverse mode")
		}
	}
	return nil
}

func validateEndpoint(raw string, schemes ...string) error {
	if raw == "" {
		return fmt.Errorf("onebot: url is required in this mode")
	}
	if !utf8.ValidString(raw) {
		return fmt.Errorf("onebot: url is not valid UTF-8")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("onebot: invalid url: %w", err)
	}
	validScheme := false
	for _, scheme := range schemes {
		if u.Scheme == scheme {
			validScheme = true
			break
		}
	}
	if !validScheme || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("onebot: url must be an absolute %s URL without userinfo or fragment", strings.Join(schemes, "/"))
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("onebot: url has an empty port")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return fmt.Errorf("onebot: url port must be a number from 1 to 65535")
		}
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		if u.RawQuery != "" {
			return fmt.Errorf("onebot: HTTP API root must not contain a query")
		}
	}
	return nil
}

func validateListenerAuth(listen, token string) error {
	if listen == "" || token != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil // SplitHostPort validation above reports the useful error.
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("onebot: accessToken is required for a non-loopback listener")
}

func validateToken(token string) error {
	if len(token) > maxTokenBytes {
		return fmt.Errorf("onebot: accessToken exceeds %d bytes", maxTokenBytes)
	}
	if !utf8.ValidString(token) {
		return fmt.Errorf("onebot: accessToken is not valid UTF-8")
	}
	if strings.TrimSpace(token) != token {
		return fmt.Errorf("onebot: accessToken must not have surrounding whitespace")
	}
	for _, r := range token {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("onebot: accessToken contains a control character")
		}
	}
	return nil
}
