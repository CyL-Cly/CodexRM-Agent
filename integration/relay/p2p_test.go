package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/p2p"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/server"
	"github.com/gorilla/websocket"
)

func directTestRM(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PSTUN: "127.0.0.1:9", P2PLocalTest: true})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); h.Close() })
	return s, h
}

func signalUntil(t *testing.T, c *websocket.Conn, kind string) protocol.Envelope {
	t.Helper()
	return until(t, c, func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeP2P && e.P2P != nil && e.P2P.Kind == kind
	})
}

func TestDirectBusinessAndFallback(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		_, h := directTestRM(t)
		testDirectBusinessAndFallback(t, h, false, false, false, false)
	})
	t.Run("both unpredictable with capability", func(t *testing.T) {
		testDirectBusinessAndFallback(t, dualUnpredictableTestRM(t), true, false, false, false)
	})
	t.Run("client UDP egress differs from signaling", func(t *testing.T) {
		s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PSTUN: "127.0.0.1:9",
			P2PTrustProxy: true, P2PLocalTest: true})
		if err != nil {
			t.Fatal(err)
		}
		handler := s.Handler()
		h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := "127.0.0.1"
			if r.URL.Path == "/api/v1/client/ws" {
				ip = "192.0.2.20" // No UDP candidate has this signaling IP or a verifier receipt.
			}
			r.Header.Set("X-Real-IP", ip)
			r.Header.Set("X-Forwarded-Proto", "https")
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(func() { s.Close(); h.Close() })
		testDirectBusinessAndFallback(t, h, false, false, false, true)
	})
	t.Run("IPv6 UDP", func(t *testing.T) {
		testDirectBusinessAndFallback(t, ipv6TestRM(t), false, true, true, false)
	})
	t.Run("IPv4 UDP verified independently of IPv6 WebSocket", func(t *testing.T) {
		s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PSTUN: "127.0.0.1:9",
			P2PTrustProxy: true, P2PLocalTest: true, P2PVerifyListen: "127.0.0.1:0", P2PVerifyPublic: "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}
		handler := s.Handler()
		h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate the TLS reverse proxy seeing different IPs from the UDP listener.
			ip := "192.0.2.10"
			if r.URL.Path == "/api/v1/client/ws" {
				ip = "2001:db8::20"
			}
			r.Header.Set("X-Real-IP", ip)
			r.Header.Set("X-Forwarded-Proto", "https")
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(func() { s.Close(); h.Close() })
		testDirectBusinessAndFallback(t, h, false, true, false, false)
	})
	t.Run("IPv6 UDP verified independently of IPv4 WebSocket", func(t *testing.T) {
		probe, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
		if err != nil {
			t.Skipf("no IPv6 UDP loopback: %v", err)
		}
		_ = probe.Close()
		s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PSTUN: "[::1]:9",
			P2PTrustProxy: true, P2PLocalTest: true, P2PVerifyListen: "[::1]:0", P2PVerifyPublic: "[::1]:0"})
		if err != nil {
			t.Fatal(err)
		}
		handler := s.Handler()
		h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("X-Real-IP", "192.0.2.10")
			r.Header.Set("X-Forwarded-Proto", "https")
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(func() { s.Close(); h.Close() })
		testDirectBusinessAndFallback(t, h, false, true, true, false)
	})
}

func ipv6TestRM(t *testing.T) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 TCP loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stun, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no IPv6 UDP loopback: %v", err)
	}
	t.Cleanup(func() { _ = stun.Close() })
	go func() {
		var req [64]byte
		for {
			n, from, err := stun.ReadFromUDPAddrPort(req[:])
			if err != nil {
				return
			}
			if n != 20 {
				continue
			}
			var reply [44]byte
			copy(reply[:], req[:n])
			binary.BigEndian.PutUint16(reply[:2], 0x0101)
			binary.BigEndian.PutUint16(reply[2:4], 24)
			binary.BigEndian.PutUint16(reply[20:22], 0x0020)
			binary.BigEndian.PutUint16(reply[22:24], 20)
			reply[25] = 2
			binary.BigEndian.PutUint16(reply[26:28], from.Port()^0x2112)
			for i, b := range from.Addr().As16() {
				reply[28+i] = b ^ req[4+i]
			}
			_, _ = stun.WriteToUDPAddrPort(reply[:], from)
		}
	}()
	s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PSTUN: stun.LocalAddr().String(), P2PLocalTest: true})
	if err != nil {
		t.Fatal(err)
	}
	// Construct directly so the unused default loopback listener isn't opened.
	h := &httptest.Server{Listener: listener, Config: &http.Server{Handler: s.Handler()}}
	h.Start()
	t.Cleanup(func() { s.Close(); h.Close() })
	return h
}

func testDirectBusinessAndFallback(t *testing.T, h *httptest.Server, dualNAT, ipv6, wantIPv6, clientInitiated bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cwd := t.TempDir()
	contents := bytes.Repeat([]byte("direct artifact"), 75_000)
	if err := os.WriteFile(filepath.Join(cwd, "sample.apk"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	const png64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aA3sAAAAASUVORK5CYII="
	png, err := base64.StdEncoding.DecodeString(png64)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "sample.png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{ServerURL: h.URL, DataDir: t.TempDir(), Cwd: cwd, Mock: true, EnrollmentKey: "test-key", AllowFileManagement: true})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := a.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	var client protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &client)
	c := ws(t, h, "client", client.Token)
	signalUntil(t, c, "available")
	readSignal := func(sig *protocol.P2PSignal) {
		write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: sig, ClientID: "spoofed"})
	}
	rpc(t, c, 1, "codexrm/info", nil)
	if response(t, c, 1)["mock"] != true {
		t.Fatal("WSS fallback not available during negotiation")
	}
	begin := protocol.P2P("begin", "")
	begin.DualNATPunch, begin.IPv6 = dualNAT, ipv6
	begin.ClientInitiated = clientInitiated
	readSignal(begin)
	start := signalUntil(t, c, "start").P2P
	if start.DualNATPunch != dualNAT {
		t.Fatalf("negotiated dual NAT punch = %t, want %t", start.DualNATPunch, dualNAT)
	}
	if start.ClientInitiated != clientInitiated {
		t.Fatalf("negotiated client initiation = %t, want %t", start.ClientInitiated, clientInitiated)
	}
	if start.IPv6 != ipv6 {
		t.Fatalf("negotiated IPv6 = %t, want %t", start.IPv6, ipv6)
	}
	s, offer, err := p2p.Prepare(ctx, false, *start)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	readSignal(offer)
	remote := signalUntil(t, c, "offer").P2P
	if dualNAT && (offer.Mapping != protocol.P2PMappingVaries || remote.Mapping != protocol.P2PMappingVaries) {
		t.Fatalf("expected both mappings to be unpredictable, got %q and %q", offer.Mapping, remote.Mapping)
	}
	ready := make(chan struct{})
	go func() {
		for {
			var e protocol.Envelope
			if err := c.ReadJSON(&e); err != nil {
				return
			}
			if e.P2P != nil && e.P2P.Kind == "ready" {
				close(ready)
				return
			}
		}
	}()
	link, err := s.Connect(ctx, *remote, ready, func() error {
		return c.WriteJSON(protocol.Envelope{Type: protocol.TypeP2P, P2P: protocol.P2P("ready", start.ID)})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	if link.IsIPv6() != wantIPv6 {
		t.Fatalf("IPv6 link = %t, want %t", link.IsIPv6(), wantIPv6)
	}
	readSignal(protocol.P2P("activate", start.ID))
	signalUntil(t, c, "active")
	direct := func(id int, method string, p any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(protocol.Envelope{Type: protocol.TypeClientMessage, ClientID: "other", Message: raw(map[string]any{"id": id, "method": method, "params": p})})
		if err := link.Write(b); err != nil {
			t.Fatal(err)
		}
		for {
			b, err := link.Read()
			if err != nil {
				t.Fatal(err)
			}
			var e protocol.Envelope
			var m map[string]any
			if json.Unmarshal(b, &e) != nil || json.Unmarshal(e.Message, &m) != nil {
				t.Fatal("invalid direct reply")
			}
			if m["id"] != float64(id) {
				continue
			}
			if e.ClientID != client.ClientID || m["error"] != nil {
				t.Fatalf("direct reply: %s", b)
			}
			return m["result"].(map[string]any)
		}
	}
	thread := direct(2, "thread/start", nil)["thread"].(map[string]any)
	if direct(3, "thread/read", map[string]any{"threadId": thread["id"]})["thread"] == nil {
		t.Fatal("missing direct thread")
	}
	fileParams := map[string]any{"workspace": cwd, "path": "sample.apk"}
	checkRelayDenied := func() {
		write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, StreamID: "blocked", Message: raw(map[string]any{"id": 991, "method": protocol.DirectFilePrefix + "open", "params": fileParams})})
		e := until(t, c, func(e protocol.Envelope) bool { return e.StreamID == "blocked" })
		if e.Type != protocol.TypeError || len(e.Message) != 0 {
			t.Fatalf("RM accepted a file request: %+v", e)
		}
	}
	checkRelayDenied()
	// The same paired client can transfer a chat attachment on the QUIC connection.
	const contents64 = "AQIDBA=="
	attachment := direct(5, "codexrm/attachment/begin", map[string]any{"name": "example.bin", "size": 4})
	attachmentID := attachment["id"]
	direct(6, "codexrm/attachment/chunk", map[string]any{"id": attachmentID, "offset": 0, "data": contents64})
	direct(7, "codexrm/attachment/finish", map[string]any{"id": attachmentID})
	if downloaded := direct(8, "codexrm/attachment/read", map[string]any{"id": attachmentID, "offset": 0}); downloaded["data"] != contents64 {
		t.Fatalf("P2P attachment differs: %+v", downloaded)
	}
	if image := direct(9, "codexrm/image/read", map[string]any{"path": "sample.png", "cwd": cwd, "offset": 0}); image["data"] != png64 {
		t.Fatalf("P2P image differs: %+v", image)
	}
	handle := direct(10, protocol.DirectFilePrefix+"open", fileParams)
	var downloaded []byte
	for id := 11; len(downloaded) < len(contents); id++ {
		chunk := direct(id, protocol.DirectFilePrefix+"read", map[string]any{"id": handle["id"], "offset": len(downloaded)})
		data, err := base64.StdEncoding.DecodeString(chunk["data"].(string))
		if err != nil || len(data) == 0 {
			t.Fatalf("invalid direct chunk: %v", err)
		}
		downloaded = append(downloaded, data...)
	}
	if !bytes.Equal(downloaded, contents) {
		t.Fatal("download differs from original")
	}
	readSignal(protocol.P2P("stop", start.ID))
	signalUntil(t, c, "stop")
	checkRelayDenied()
	rpc(t, c, 4, "thread/list", nil)
	if len(response(t, c, 4)["data"].([]any)) != 1 {
		t.Fatal("WSS did not retain state after fallback")
	}
}

func dualUnpredictableTestRM(t *testing.T) *httptest.Server {
	t.Helper()
	// One STUN destination reports the usable loopback socket, the other a
	// distant mapped port. Both clients classify their mapping as unpredictable.
	var endpoints []string
	for _, alternate := range []bool{true, false} {
		stun, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stun.Close() })
		endpoints = append(endpoints, stun.LocalAddr().String())
		go func() {
			var buf [64]byte
			for {
				n, from, err := stun.ReadFromUDP(buf[:])
				if err != nil {
					return
				}
				if n < 20 {
					continue
				}
				var reply [32]byte
				copy(reply[:], buf[:20])
				binary.BigEndian.PutUint16(reply[:2], 0x0101) // Binding success
				binary.BigEndian.PutUint16(reply[2:4], 12)
				binary.BigEndian.PutUint16(reply[20:22], 0x0020) // XOR-MAPPED-ADDRESS
				binary.BigEndian.PutUint16(reply[22:24], 8)
				reply[25] = 1
				port := uint16(from.Port)
				if alternate {
					port = 1024 + uint16((uint32(port)+28000)%(65536-1024))
				}
				binary.BigEndian.PutUint16(reply[26:28], port^0x2112)
				binary.BigEndian.PutUint32(reply[28:32], 0x7f000001^0x2112a442)
				_, _ = stun.WriteToUDP(reply[:], from)
			}
		}()
	}
	s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PSTUN: strings.Join(endpoints, ","), P2PLocalTest: true})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); h.Close() })
	return h
}

func TestDirectDualUnpredictableMappingsFallBackToRelay(t *testing.T) {
	t.Run("legacy", func(t *testing.T) { testDualNATFallback(t, false) })
	t.Run("negotiated timeout", func(t *testing.T) { testDualNATFallback(t, true) })
}

func testDualNATFallback(t *testing.T, dualNAT bool) {
	t.Helper()
	h := dualUnpredictableTestRM(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
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
	t.Cleanup(func() { cancel(); <-done })
	var client protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &client)
	c := ws(t, h, "client", client.Token)
	signalUntil(t, c, "available")
	begin := protocol.P2P("begin", "")
	begin.DualNATPunch = dualNAT
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: begin})
	start := signalUntil(t, c, "start").P2P
	if start.DualNATPunch != dualNAT {
		t.Fatalf("negotiated dual NAT punch = %t, want %t", start.DualNATPunch, dualNAT)
	}
	if fromAgent := signalUntil(t, c, "offer").P2P; fromAgent.Mapping != protocol.P2PMappingVaries {
		t.Fatalf("unexpected Agent mapping classification: %q", fromAgent.Mapping)
	}
	blackhole, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	offer := protocol.P2P("offer", start.ID)
	offer.Pin = strings.Repeat("ab", 32)
	offer.Candidates = []string{blackhole.LocalAddr().String()}
	offer.Mapping = protocol.P2PMappingVaries
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: offer})
	if stop := signalUntil(t, c, "stop").P2P; stop.ID != start.ID || stop.Reason != protocol.P2PReasonDualUnpredictableNAT {
		t.Fatalf("unexpected stop: %+v", stop)
	}
	rpc(t, c, 1, "codexrm/info", nil)
	if response(t, c, 1)["mock"] != true {
		t.Fatal("RM no longer relays requests")
	}
}

func TestDirectSignalingRejectsUnrelatedCandidatesAndRevokes(t *testing.T) {
	_, h := directTestRM(t)
	var a protocol.EnrollResponse
	post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{Name: "agent", InstallationID: "p2p-test"}, &a)
	var pair protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", a.Token, "", protocol.PairStartRequest{ManualCode: true}, &pair)
	var client protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &client)
	ag, c := ws(t, h, "agent", a.Token), ws(t, h, "client", client.Token)
	write(t, ag, protocol.Envelope{Type: protocol.TypeP2P, P2P: protocol.P2P("hello", "")})
	signalUntil(t, c, "available")
	begin := protocol.P2P("begin", "")
	begin.DualNATPunch, begin.ClientInitiated = true, true
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: begin, ClientID: "other"})
	start, clientStart := signalUntil(t, ag, "start"), signalUntil(t, c, "start")
	if start.P2P.DualNATPunch || clientStart.P2P.DualNATPunch {
		t.Fatal("client capability alone must not enable dual NAT punching")
	}
	if start.P2P.ClientInitiated || clientStart.P2P.ClientInitiated {
		t.Fatal("client capability alone must not permit an addressless offer")
	}
	if start.ClientID != client.ClientID {
		t.Fatal("RM trusted a supplied client ID")
	}
	offer := protocol.P2P("offer", start.P2P.ID)
	offer.Pin = strings.Repeat("ab", 32)
	offer.Candidates = []string{"192.0.2.1:33333", "127.0.0.1:33333"}
	offer.Mapping = protocol.P2PMappingLinear
	offer.Step = 4
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: offer})
	forward := signalUntil(t, ag, "offer").P2P
	if forward.Mapping != protocol.P2PMappingLinear || forward.Step != 4 {
		t.Fatalf("mapping hint = %q step %d", forward.Mapping, forward.Step)
	}
	filtered := forward.Candidates
	if len(filtered) != 1 || filtered[0] != "127.0.0.1:33333" {
		t.Fatalf("unverified target forwarded: %v", filtered)
	}
	req, _ := http.NewRequest(http.MethodDelete, h.URL+"/api/v1/agent/clients/"+client.ClientID, nil)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if signalUntil(t, ag, "stop").P2P.ID != start.P2P.ID {
		t.Fatal("wrong generation stopped")
	}
}

func TestLANOnlyOfferExchangesPeerPrivateAddress(t *testing.T) {
	s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PLAN: true, P2PTrustProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	defer s.Close()
	var a protocol.EnrollResponse
	post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{Name: "agent", InstallationID: "lan-test"}, &a)
	var pair protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", a.Token, "", protocol.PairStartRequest{ManualCode: true}, &pair)
	var client protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &client)
	connect := func(role, token, ip string) *websocket.Conn {
		t.Helper()
		headers := http.Header{"Authorization": {"Bearer " + token}, "X-Forwarded-Proto": {"https"}, "X-Real-Ip": {ip}}
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/api/v1/"+role+"/ws", headers)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	ag, c := connect("agent", a.Token, "192.168.2.164"), connect("client", client.Token, "192.168.2.126")
	hello := protocol.P2P("hello", "")
	hello.LAN = true
	write(t, ag, protocol.Envelope{Type: protocol.TypeP2P, P2P: hello})
	signalUntil(t, c, "available")
	begin := protocol.P2P("begin", "")
	begin.LAN = true
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: begin})
	start := signalUntil(t, c, "start").P2P
	if !start.LAN || len(start.STUN) != 0 || start.VerifyUDP != "" {
		t.Fatalf("unexpected start: %+v", start)
	}
	offer := protocol.P2P("offer", start.ID)
	offer.Pin = strings.Repeat("ab", 32)
	offer.Local = []string{"192.168.2.126:30303", "192.168.2.123:30303", "127.0.0.1:30303", "8.8.8.8:30303"}
	offer.Candidates = []string{"192.168.2.126:30303"}
	offer.Mapping = "unrecognized"
	offer.Step = 4
	write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: offer})
	forward := signalUntil(t, ag, "offer").P2P
	if forward.Mapping != "" || forward.Step != 0 {
		t.Fatalf("forwarded an unrecognized mapping hint: %q step %d", forward.Mapping, forward.Step)
	}
	if len(forward.Candidates) != 0 || len(forward.Local) != 1 || forward.Local[0] != offer.Local[0] {
		t.Fatalf("unexpected forwarded candidates: public=%v local=%v", forward.Candidates, forward.Local)
	}
}

func TestDirectOfferUsesUDPSourceDifferentFromSignaling(t *testing.T) {
	for _, scenario := range []string{"verified", "wrong port", "wrong peer", "no receipt", "one verifier multiple sockets", "multiple verifiers", "wrong extra port"} {
		t.Run(scenario, func(t *testing.T) {
			config := server.Config{DataDir: t.TempDir(), AdminToken: "test-key", P2PSTUN: "127.0.0.1:9",
				P2PTrustProxy: true, P2PLocalTest: true, P2PVerifyListen: "127.0.0.1:0", P2PVerifyPublic: "127.0.0.1:0"}
			if scenario == "multiple verifiers" || scenario == "wrong extra port" {
				config.P2PVerifyKeyFile = filepath.Join(t.TempDir(), "verifier.key")
				if err := os.WriteFile(config.P2PVerifyKeyFile, []byte(strings.Repeat("42", 32)), 0600); err != nil {
					t.Fatal(err)
				}
				extra, err := p2p.ListenVerifier(netip.MustParseAddrPort("127.0.0.1:0"), config.P2PVerifyKeyFile)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(extra.Close)
				config.P2PVerifyMore = extra.Addr().String()
			}
			s, err := server.New(config)
			if err != nil {
				t.Fatal(err)
			}
			h := httptest.NewServer(s.Handler())
			defer h.Close()
			defer s.Close()
			var a protocol.EnrollResponse
			post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{Name: "agent", InstallationID: "proof-test"}, &a)
			var pair protocol.PairStartResponse
			post(t, h, "/api/v1/agent/pair", a.Token, "", protocol.PairStartRequest{ManualCode: true}, &pair)
			var client protocol.PairClaimResponse
			post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &client)
			connect := func(role, token, ip string) *websocket.Conn {
				t.Helper()
				headers := http.Header{}
				headers.Set("Authorization", "Bearer "+token)
				headers.Set("X-Forwarded-Proto", "https")
				headers.Set("X-Real-IP", ip)
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/api/v1/"+role+"/ws", headers)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				return conn
			}
			ag, c := connect("agent", a.Token, "192.0.2.1"), connect("client", client.Token, "192.0.2.2")
			write(t, ag, protocol.Envelope{Type: protocol.TypeP2P, P2P: protocol.P2P("hello", "")})
			signalUntil(t, c, "available")
			write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: protocol.P2P("begin", "")})
			agentStart, start := signalUntil(t, ag, "start").P2P, signalUntil(t, c, "start").P2P
			if start.Ticket == "" || start.Ticket == agentStart.Ticket {
				t.Fatal("expected separate tickets")
			}
			if scenario == "wrong peer" {
				start.Ticket = agentStart.Ticket
			}
			start.STUN = nil
			transport, offer, err := p2p.Prepare(t.Context(), true, *start)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.Close()
			if offer.Proof == "" || !strings.HasPrefix(offer.Candidates[0], "127.0.0.1:") {
				t.Fatal("expected receipt for the real UDP socket")
			}
			verified := offer.Candidates[0]
			if config.P2PVerifyMore != "" || scenario == "one verifier multiple sockets" {
				if !start.MultiPath || offer.Proofs[verified] == "" || (config.P2PVerifyMore != "" && (len(start.VerifyMore) != 1 || start.VerifyMore[0] != config.P2PVerifyMore)) {
					t.Fatal("expected additional verifier and deduplicated receipt from the punching socket")
				}
				// Use a second source socket to simulate an alternate mapping returned by the remote verifier.
				alternate := *start
				if len(start.VerifyMore) > 0 {
					alternate.VerifyUDP = start.VerifyMore[0]
				}
				alternate.VerifyMore = nil
				otherSocket, otherOffer, err := p2p.Prepare(t.Context(), false, alternate)
				if err != nil {
					t.Fatal(err)
				}
				defer otherSocket.Close()
				if otherOffer.Proof == "" || len(otherOffer.Candidates) == 0 {
					t.Fatal("remote verifier did not issue a receipt")
				}
				other := otherOffer.Candidates[0]
				if scenario == "wrong extra port" {
					addr := netip.MustParseAddrPort(other)
					wrong := addr.Port()%65535 + 1
					if netip.AddrPortFrom(addr.Addr(), wrong).String() == verified {
						wrong = wrong%65535 + 1
					}
					other = netip.AddrPortFrom(addr.Addr(), wrong).String()
				}
				offer.Candidates = []string{verified, other}
				offer.Proofs[other] = otherOffer.Proof
				offer.PathMappings = map[string]protocol.P2PMappingHint{
					verified: {Mapping: protocol.P2PMappingLinear, Step: 3},
					other:    {Mapping: protocol.P2PMappingLinear, Step: 99},
				}

				write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: offer})
				forwarded := signalUntil(t, ag, "offer").P2P
				want := []string{verified}
				if scenario == "multiple verifiers" || scenario == "one verifier multiple sockets" {
					want = append(want, other)
				}
				if strings.Join(forwarded.Candidates, ",") != strings.Join(want, ",") || forwarded.Proof != "" || len(forwarded.Proofs) != 0 || forwarded.Ticket != "" {
					t.Fatalf("wrong forwarded candidates or leaked receipts: %+v", forwarded)
				}
				if len(forwarded.PathMappings) != len(want) || forwarded.PathMappings[verified].Step != 3 {
					t.Fatalf("unexpected verified mappings: %+v", forwarded.PathMappings)
				}
				if len(want) > 1 && forwarded.PathMappings[other].Mapping != protocol.P2PMappingVaries {
					t.Fatalf("invalid port step was not normalized: %+v", forwarded.PathMappings)
				}

				return
			}
			addr, err := net.ResolveUDPAddr("udp4", verified)
			if err != nil {
				t.Fatal(err)
			}
			addr.Port = addr.Port%65535 + 1
			if scenario == "wrong port" {
				offer.Candidates = []string{addr.String()}
			} else {
				offer.Candidates = []string{verified, addr.String(), "192.0.2.99:40000"}
			}
			if scenario == "no receipt" {
				offer.Proof, offer.Proofs = "", nil
			}
			write(t, c, protocol.Envelope{Type: protocol.TypeP2P, P2P: offer})
			if scenario == "verified" {
				forwarded := signalUntil(t, ag, "offer").P2P
				if len(forwarded.Candidates) != 1 || forwarded.Candidates[0] != verified || forwarded.Proof != "" || forwarded.Ticket != "" {
					t.Fatalf("forwarded wrong candidate or leaked receipt: %+v", forwarded)
				}
			} else if stop := signalUntil(t, c, "stop").P2P; stop.Reason != "client_candidate_unverified" {
				t.Fatalf("unexpected stop reason: %s", stop.Reason)
			}
		})
	}
}
