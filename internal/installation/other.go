//go:build !windows

package installation

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
)

func stopLegacy(context.Context, string) error { return nil }
func MigrateLegacy(string) error               { return nil }

func decodeJSON(b []byte, v any) error          { return json.Unmarshal(b, v) }
func hide(*exec.Cmd)                            {}
func waitStopped(context.Context, string) error { return nil }
func register(string, Options) error            { return errors.New("安装器仅支持 Windows") }
func unregister(string) error                   { return errors.New("安装器仅支持 Windows") }
