package agent

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/p2p"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/quic-go/quic-go"
)

func (a *Agent) serveFileDownload(d *directSession, files *directFiles, stream *quic.Stream) {
	defer stream.CancelRead(0)
	var id [protocol.DirectShareIDSize]byte
	if _, err := io.ReadFull(stream, id[:]); err != nil || !canonicalTransferID(string(id[:])) {
		stream.CancelWrite(1)
		return
	}
	// The client does not send a body. Enforce the stream framing before using
	// the ID to look up a file already authorized on this exact direct session.
	var trailing [1]byte
	if _, err := io.ReadFull(stream, trailing[:]); err != io.EOF || !a.beginRequest() {
		stream.CancelWrite(1)
		return
	}
	defer a.endRequest()
	_ = stream.SetWriteDeadline(time.Now().Add(protocol.DirectShareTimeout))
	if err := files.sendStream(d.ctx, string(id[:]), stream); err != nil {
		stream.CancelWrite(1)
	}
}

type directDownloadStream interface {
	io.WriteCloser
	CancelWrite(quic.StreamErrorCode)
}

func (s *directFiles) sendStream(ctx context.Context, id string, stream directDownloadStream) (err error) {
	s.mu.Lock()
	r := s.read
	if r == nil || r.id != id || !r.streamed || r.abortStream != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return errors.New("下载已结束，请重试")
	}
	r.abortStream = func() { stream.CancelWrite(1) }
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.read != r {
			err = errors.New("下载已取消")
			return
		}
		// No FIN on a changed, short or canceled file. The receiver only
		// publishes the file after the complete body and a clean EOF.
		if err == nil {
			err = stream.Close()
		}
		r.abortStream = nil
		s.closeRead()
	}()

	if err := p2p.CopyFilePayload(stream, r.file, r.info.Size()); err != nil {
		return err
	}
	if info, err := r.file.Stat(); err != nil || info.Size() != r.info.Size() || !info.ModTime().Equal(r.info.ModTime()) {
		return errors.New("文件已修改，请重新下载")
	}
	return ctx.Err()
}
