package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestBrowserCanUpdateItsPairedGPUProfile(t *testing.T) {
	_, h := newServer(t)
	var enrolled protocol.EnrollResponse
	post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{InstallationID: "gpu-test"}, &enrolled)
	var pair protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", enrolled.Token, "", map[string]any{}, &pair)
	var claimed protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode, ClientName: "CodexRM Web"}, &claimed)
	profile := protocol.ClientProfileRequest{ClientName: "ANGLE (AMD, AMD Radeon 780M Graphics (0x00001900) Direct3D11 vs_5_0 ps_5_0, D3D11)", DeviceType: "desktop"}
	if status := post(t, h, "/api/v1/client/profile", "invalid", "", profile, nil); status != 401 {
		t.Fatalf("unauthorized profile update: %d", status)
	}
	if status := post(t, h, "/api/v1/client/profile", claimed.Token, "", profile, nil); status != 200 {
		t.Fatalf("profile update: %d", status)
	}
	req, _ := http.NewRequest("GET", h.URL+"/api/v1/agent/clients", nil)
	req.Header.Set("Authorization", "Bearer "+enrolled.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Data []protocol.ClientInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 || listed.Data[0].ClientID != claimed.ClientID || listed.Data[0].ClientName != profile.ClientName || listed.Data[0].DeviceType != "desktop" {
		t.Fatalf("agent device list did not receive the complete GPU profile: %+v", listed.Data)
	}
	for _, bad := range []protocol.ClientProfileRequest{{DeviceType: "desktop"}, {ClientName: strings.Repeat("x", 1025), DeviceType: "desktop"}, {ClientName: "GPU", DeviceType: "invalid"}} {
		if status := post(t, h, "/api/v1/client/profile", claimed.Token, "", bad, nil); status != 400 {
			t.Fatalf("invalid profile accepted: %+v, %d", bad, status)
		}
	}
	post(t, h, "/api/v1/client/revoke", claimed.Token, "", map[string]any{}, nil)
	if status := post(t, h, "/api/v1/client/profile", claimed.Token, "", profile, nil); status != 401 {
		t.Fatalf("revoked profile update: %d", status)
	}
}
