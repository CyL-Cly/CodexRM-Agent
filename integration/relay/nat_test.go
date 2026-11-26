package server_test

import (
	"context"
	"encoding/binary"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/p2p"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/server"
	"github.com/google/uuid"
)

func TestNATDiagnosticThroughRelayWithoutDirect(t *testing.T) {
	mockSTUN, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer mockSTUN.Close()
	go func() {
		var b [32]byte
		for {
			n, from, err := mockSTUN.ReadFromUDPAddrPort(b[:])
			if err != nil {
				return
			}
			if n != 20 {
				continue
			}
			binary.BigEndian.PutUint16(b[0:2], 0x101)
			binary.BigEndian.PutUint16(b[2:4], 12)
			binary.BigEndian.PutUint16(b[20:22], 1)
			binary.BigEndian.PutUint16(b[22:24], 8)
			b[24], b[25] = 0, 1
			binary.BigEndian.PutUint16(b[26:28], from.Port())
			copy(b[28:], from.Addr().AsSlice())
			_, _ = mockSTUN.WriteToUDPAddrPort(b[:], from)
		}
	}()
	// TCP source IP is not trusted (plain HTTP), and no P2P is enabled.
	// The two tools use RM's dedicated NAT test STUN configuration.
	s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PNATSTUN: mockSTUN.LocalAddr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	a, err := agent.New(agent.Config{ServerURL: h.URL, DataDir: t.TempDir(), Cwd: t.TempDir(), Mock: true, EnrollmentKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := a.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	var client protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &client)
	c := ws(t, h, "client", client.Token)
	until(t, c, func(e protocol.Envelope) bool { return e.Status == "connected" })
	rpc(t, c, 1, "codexrm/info", nil)
	_ = response(t, c, 1) // The Agent hello precedes this response.
	request := protocol.P2P("diagnose", uuid.NewString())
	request.STUN = []string{"192.0.2.99:9999"} // Not authorized to choose endpoints.
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: request})
	start := signalUntil(t, c, "diagnose_start").P2P
	if start.ID != request.ID || start.NATRetryAfterMs != protocol.NATDiagnosticWindow.Milliseconds() || start.NATUsesP2PSTUN || len(start.STUN) != 1 || start.STUN[0] != mockSTUN.LocalAddr().String() {
		t.Fatalf("wrong diagnostic start: %+v", start)
	}
	local := p2p.DiagnoseNAT(ctx, start.STUN)
	remote := signalUntil(t, c, "diagnose_result").P2P
	want := protocol.NATReport{RFC3489: protocol.NATUnsupported, Mapping: protocol.NATUnsupported, Filtering: protocol.NATUnsupported}
	if remote.ID != request.ID || remote.NATResult == nil || *remote.NATResult != want || local != want {
		t.Fatalf("diagnostic local=%+v remote=%+v", local, remote.NATResult)
	}
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: protocol.P2P("diagnose", uuid.NewString())})
	if busy := signalUntil(t, c, "diagnose_error").P2P; busy.Reason != "diagnose_busy" || busy.NATRetryAfterMs <= 0 || busy.NATRetryAfterMs > protocol.NATDiagnosticWindow.Milliseconds() {
		t.Fatalf("expected rate limit, got %+v", busy)
	}
	rpc(t, c, 2, "codexrm/info", nil)
	if response(t, c, 2)["mock"] != true {
		t.Fatal("diagnostic disturbed the relay")
	}
}
