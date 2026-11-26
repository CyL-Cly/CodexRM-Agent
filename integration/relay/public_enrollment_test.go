package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/server"
)

func TestPublicEnrollmentDoesNotGrantDeviceAccess(t *testing.T) {
	s, err := server.New(server.Config{DataDir: t.TempDir(), PublicEnrollment: true})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); h.Close() })

	enroll := "/api/v1/agent/enroll"
	req := protocol.EnrollRequest{InstallationID: "test-public"}
	var agent protocol.EnrollResponse
	if status := post(t, h, enroll, "", "", req, &agent); status != http.StatusOK || agent.Token == "" {
		t.Fatalf("public enrollment status = %d, want 200 and an agent token", status)
	}
	if status := post(t, h, enroll, "", "", req, nil); status != http.StatusUnauthorized {
		t.Fatalf("takeover status = %d, want 401", status)
	}
	for _, token := range []string{"", agent.Token} {
		req, err := http.NewRequest(http.MethodGet, h.URL+"/api/v1/agents", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("list status = %d, want 401", resp.StatusCode)
		}
	}
	if status := post(t, h, "/api/v1/agent/pair", "", "", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pair status = %d, want 401", status)
	}
	var pair protocol.PairStartResponse
	if status := post(t, h, "/api/v1/agent/pair", agent.Token, "", nil, &pair); status != http.StatusOK || pair.PairingCode == "" {
		t.Fatalf("authenticated pair status = %d, want 200 and a pairing code", status)
	}
	if status := post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.PairingCode}, nil); status != http.StatusOK {
		t.Fatalf("claim status = %d, want 200", status)
	}
	req.InstallationID = "test-oversize"
	req.Name = strings.Repeat("x", 8<<10)
	if status := post(t, h, enroll, "", "", req, nil); status != http.StatusBadRequest {
		t.Fatalf("oversize enrollment status = %d, want 400", status)
	}
}

func TestPublicEnrollmentRejectsAdminKey(t *testing.T) {
	if s, err := server.New(server.Config{DataDir: t.TempDir(), PublicEnrollment: true, AdminToken: "test-key"}); err == nil {
		s.Close()
		t.Fatal("public enrollment accepted an admin key")
	}
}
