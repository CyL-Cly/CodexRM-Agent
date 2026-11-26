package agenthost

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const settingsAccessFile = "settings-access.json"
const settingsSessionLifetime = 48 * time.Hour

type settingsAccess struct {
	Port     int    `json:"port"`
	Password string `json:"password"`
}

func settingsSecret() string {
	var secret [32]byte
	rand.Read(secret[:])
	return base64.RawURLEncoding.EncodeToString(secret[:])
}

func signSettingsSession(secret string, expiresAt int64) string {
	expiry := strconv.FormatInt(expiresAt, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("settings-session:" + expiry))
	return expiry + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validSettingsSession(secret, token string, now time.Time) bool {
	expiry, _, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	expiresAt, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && now.Unix() < expiresAt && hmac.Equal([]byte(token), []byte(signSettingsSession(secret, expiresAt)))
}

func readSettingsAccess(path string) (settingsAccess, error) {
	var access settingsAccess
	info, err := os.Stat(path)
	if err != nil {
		return access, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS == "linux" && info.Mode().Perm()&0077 != 0) {
		return access, fmt.Errorf("expected a private settings access file: %s (mode 0600)", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return access, err
	}
	if err := json.Unmarshal(data, &access); err != nil {
		return access, fmt.Errorf("invalid settings access JSON: %s", path)
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(access.Password)
	if access.Port < 1 || access.Port > 65535 || err != nil || len(secret) != 32 {
		return access, fmt.Errorf("invalid port or password in %s", path)
	}
	return access, nil
}

func createSettingsAccess(path string, access settingsAccess) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".settings-access-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(access)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Publish completely written credentials without replacing an existing file.
	return os.Link(file.Name(), path)
}

func listenPasswordSettings(path, bindHost string) (net.Listener, string, error) {
	access, err := readSettingsAccess(path)
	if errors.Is(err, os.ErrNotExist) {
		listener, listenErr := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
		if listenErr != nil {
			return nil, "", listenErr
		}
		access = settingsAccess{Port: listener.Addr().(*net.TCPAddr).Port, Password: settingsSecret()}
		if err := createSettingsAccess(path, access); err == nil {
			return listener, access.Password, nil
		} else if !errors.Is(err, os.ErrExist) {
			listener.Close()
			return nil, "", err
		}
		listener.Close()
		access, err = readSettingsAccess(path)
	}
	if err != nil {
		return nil, "", err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(access.Port)))
	return listener, access.Password, err
}
