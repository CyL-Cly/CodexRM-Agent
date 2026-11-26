package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/p2p"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/quic-go/quic-go"
)

const downloadTestOwner = "android-test"

func directDownloadTestQUIC(t testing.TB, window uint64) (context.Context, *quic.Conn, *quic.Conn) {
	t.Helper()
	ctx := t.Context()
	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cert := httptest.NewTLSServer(nil)
	serverTLS := cert.TLS.Clone()
	cert.Close()
	serverTLS.NextProtos = []string{"codexrm-file-test"}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, &quic.Config{MaxIncomingStreams: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	clientTLS := cert.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	clientTLS.NextProtos = serverTLS.NextProtos
	client, err := quic.DialAddr(handshakeCtx, listener.Addr().String(), clientTLS, &quic.Config{
		InitialStreamReceiveWindow: window, MaxStreamReceiveWindow: window,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CloseWithError(0, "") })
	server, err := listener.Accept(handshakeCtx)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, client, server
}

func stageDirectDownload(t testing.TB, path string, data []byte) (*Agent, PhoneTransfer) {
	t.Helper()
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	item, err := a.QueuePhoneFile(downloadTestOwner, path, "Android test")
	if err != nil {
		t.Fatal(err)
	}
	return a, item
}

func openDirectDownloadTest(t testing.TB, a *Agent, d *directSession, item PhoneTransfer, stream bool) string {
	t.Helper()
	value, err := a.phoneTransferRPC(d.ctx, d.client, protocol.PhoneTransferPrefix+"open", map[string]any{"id": item.ID, "downloadStream": stream}, d)
	if err != nil {
		t.Fatal(err)
	}
	header := value.(map[string]any)
	if stream && header["downloadStream"] != true || !stream && header["downloadStream"] != nil {
		t.Fatalf("unexpected negotiation: %v", header)
	}
	return header["id"].(string)
}

func requestDirectDownloadTest(t testing.TB, ctx context.Context, client, server *quic.Conn, a *Agent, d *directSession, request string, extraDelay time.Duration) *quic.Stream {
	t.Helper()
	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(stream, request); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	peer, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if extraDelay > 0 {
			time.Sleep(extraDelay)
		}
		a.serveFileStream(d, peer)
	}()
	return stream
}

func TestPhoneBinaryDownloadQUIC(t *testing.T) {
	connCtx, client, server := directDownloadTestQUIC(t, 0)
	ctx, cancel := context.WithTimeout(connCtx, 10*time.Second)
	defer cancel()
	data := bytes.Repeat([]byte("desktop→phone\x00"), 50_000)
	a, item := stageDirectDownload(t, filepath.Join(t.TempDir(), "example.bin"), data)
	d := &directSession{ctx: ctx, client: downloadTestOwner}
	defer d.phoneFiles.close()
	id := openDirectDownloadTest(t, a, d, item, true)
	if _, err := a.phoneTransferRPC(ctx, d.client, protocol.PhoneTransferPrefix+"read", map[string]any{"id": id, "offset": float64(0)}, d); err == nil {
		t.Fatal("binary read also accepted legacy chunks")
	}
	phoneRequest := string(protocol.PhoneFileDownload) + id
	for _, test := range []struct {
		request string
		session *directSession
	}{
		{phoneRequest, &directSession{ctx: ctx, client: d.client}}, // same phone, different session
		{string(protocol.DirectFileDownload) + id, d},              // wrong handle namespace
		{phoneRequest + "x", d},
		{phoneRequest[:len(phoneRequest)-1], d},
	} {
		stream := requestDirectDownloadTest(t, ctx, client, server, a, test.session, test.request, 0)
		var reset *quic.StreamError
		if err := p2p.ReceiveFilePayload(io.Discard, stream, item.Size); !errors.As(err, &reset) {
			t.Fatalf("invalid request was not reset: %v", err)
		}
	}
	var got bytes.Buffer
	stream := requestDirectDownloadTest(t, ctx, client, server, a, d, phoneRequest, 0)
	if err := p2p.ReceiveFilePayload(&got, stream, item.Size); err != nil || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("download mismatch (%d bytes): %v", got.Len(), err)
	}
	if _, err := a.phoneTransferRPC(ctx, d.client, protocol.PhoneTransferPrefix+"ack", map[string]any{"id": item.ID}, d); err != nil {
		t.Fatal(err)
	}
	if remaining, err := a.PendingPhoneTransfers(); err != nil || len(remaining) != 0 {
		t.Fatalf("ACK left cache: %+v, %v", remaining, err)
	}
}

func TestWorkspaceBinaryDownloadQUIC(t *testing.T) {
	connCtx, client, server := directDownloadTestQUIC(t, 0)
	ctx, cancel := context.WithTimeout(connCtx, 10*time.Second)
	defer cancel()
	workspace := t.TempDir()
	const name = "workspace-output.bin"
	data := bytes.Repeat([]byte("file\x00"), 80_000)
	if err := os.WriteFile(filepath.Join(workspace, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{Cwd: workspace, DataDir: t.TempDir(), AllowFileManagement: true}}
	d := &directSession{ctx: ctx, client: downloadTestOwner}
	defer d.files.close()
	opened, err := a.directFileRPC(ctx, nil, d, protocol.DirectFilePrefix+"open", map[string]any{
		"workspace": workspace, "path": name, "downloadStream": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	header := opened.(map[string]any)
	if header["downloadStream"] != true {
		t.Fatalf("stream not negotiated: %v", header)
	}
	stream := requestDirectDownloadTest(t, ctx, client, server, a, d, string(protocol.DirectFileDownload)+header["id"].(string), 0)
	var got bytes.Buffer
	if err := p2p.ReceiveFilePayload(&got, stream, header["size"].(int64)); err != nil || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("workspace download mismatch (%d bytes): %v", got.Len(), err)
	}
}

func TestCancelActivePhoneBinaryDownload(t *testing.T) {
	connCtx, client, server := directDownloadTestQUIC(t, 8<<10)
	ctx, cancel := context.WithTimeout(connCtx, 10*time.Second)
	defer cancel()
	data := bytes.Repeat([]byte("pending bytes"), 100_000)
	original := filepath.Join(t.TempDir(), "example.bin")
	a, item := stageDirectDownload(t, original, data)
	d := &directSession{ctx: ctx, client: downloadTestOwner}
	defer d.phoneFiles.close()
	id := openDirectDownloadTest(t, a, d, item, true)
	stream := requestDirectDownloadTest(t, ctx, client, server, a, d, string(protocol.PhoneFileDownload)+id, 0)
	var first [1]byte
	if _, err := io.ReadFull(stream, first[:]); err != nil || first[0] != data[0] {
		t.Fatalf("download did not start: %v", err)
	}
	if err := a.CancelPhoneTransfer(filepath.Base(a.phoneTransferDir(d.client, "outbox")), item.ID); err != nil {
		t.Fatal(err)
	}
	var reset *quic.StreamError
	if err := p2p.ReceiveFilePayload(io.Discard, stream, item.Size-1); !errors.As(err, &reset) {
		t.Fatalf("the canceled transfer did not reset: %v", err)
	}
	if got, err := os.ReadFile(original); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("original file changed: %v", err)
	}
	if _, err := a.phoneTransferRPC(ctx, d.client, protocol.PhoneTransferPrefix+"open", map[string]any{"id": item.ID}, d); err == nil {
		t.Fatal("canceled transfer reopened")
	}
}

// The benchmark uses one actual loopback QUIC connection in each case. The
// 20ms delay is applied once per application request, not per QUIC packet.
func BenchmarkPhoneDownloadQUIC(b *testing.B) {
	const delay = 20 * time.Millisecond
	data := bytes.Repeat([]byte{0xa5}, 8<<20)
	for _, streamed := range []bool{false, true} {
		name := "legacy"
		if streamed {
			name = "binary"
		}
		b.Run(name, func(b *testing.B) {
			ctx, client, server := directDownloadTestQUIC(b, 0)
			a, item := stageDirectDownload(b, filepath.Join(b.TempDir(), "benchmark.bin"), data)
			d := &directSession{ctx: ctx, client: downloadTestOwner}
			defer d.phoneFiles.close()
			var control *quic.Stream
			if !streamed {
				var err error
				control, err = client.OpenStreamSync(ctx)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.SetBytes(item.Size)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				id := openDirectDownloadTest(b, a, d, item, streamed)
				b.StartTimer()
				if streamed {
					stream := requestDirectDownloadTest(b, ctx, client, server, a, d, string(protocol.PhoneFileDownload)+id, delay)
					if err := p2p.ReceiveFilePayload(io.Discard, stream, item.Size); err != nil {
						b.Fatal(err)
					}
					continue
				}
				_ = control.SetDeadline(time.Now().Add(10 * time.Second))
				if i == 0 {
					go serveLegacyDownloadBenchmark(ctx, server, a, d, delay)
				}
				for offset := int64(0); offset < item.Size; {
					if err := benchmarkFrameWrite(control, map[string]any{"id": id, "offset": offset}); err != nil {
						b.Fatal(err)
					}
					var chunk struct {
						Data   string
						Offset int64
					}
					if err := benchmarkFrameRead(control, &chunk); err != nil {
						b.Fatal(err)
					}
					data, err := base64.StdEncoding.DecodeString(chunk.Data)
					if err != nil || chunk.Offset != offset+int64(len(data)) || len(data) == 0 {
						b.Fatalf("invalid benchmark chunk: %v", err)
					}
					offset = chunk.Offset
				}
			}
		})
	}
}

func serveLegacyDownloadBenchmark(ctx context.Context, server *quic.Conn, a *Agent, d *directSession, delay time.Duration) {
	stream, err := server.AcceptStream(ctx)
	if err != nil {
		return
	}
	defer stream.CancelWrite(1)
	for {
		var params map[string]any
		if benchmarkFrameRead(stream, &params) != nil {
			return
		}
		time.Sleep(delay)
		reply, err := a.phoneTransferRPC(ctx, d.client, protocol.PhoneTransferPrefix+"read", params, d)
		if err != nil || benchmarkFrameWrite(stream, reply) != nil {
			return
		}
	}
}

func benchmarkFrameWrite(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	_, err = w.Write(append(header[:], body...))
	return err
}

func benchmarkFrameRead(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > protocol.MaxEnvelopeSize {
		return errors.New("invalid benchmark frame")
	}
	body := make([]byte, int(n))
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
