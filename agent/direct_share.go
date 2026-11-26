package agent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/p2p"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/quic-go/quic-go"
)

func (a *Agent) serveDirectShares(d *directSession, link *p2p.Link) {
	for {
		stream, err := link.AcceptStream(d.ctx)
		if err != nil {
			return
		}
		go a.serveFileStream(d, stream)
	}
}

// The connection's incoming-stream limit bounds concurrent file streams.
func (a *Agent) serveFileStream(d *directSession, stream *quic.Stream) {
	_ = stream.SetReadDeadline(time.Now().Add(15 * time.Second))
	var first [1]byte
	if _, err := io.ReadFull(stream, first[:]); err != nil {
		stream.CancelRead(0)
		stream.CancelWrite(1)
		return
	}
	switch first[0] {
	case protocol.DirectFileDownload:
		a.serveFileDownload(d, &d.files, stream)
	case protocol.PhoneFileDownload:
		a.serveFileDownload(d, &d.phoneFiles, stream)
	default:
		a.finishDirectShare(d, stream, first[0])
	}
}

func (a *Agent) finishDirectShare(d *directSession, stream *quic.Stream, first byte) {
	defer stream.CancelRead(0)
	_ = stream.SetDeadline(time.Now().Add(15 * time.Second))
	var id [protocol.DirectShareIDSize]byte
	id[0] = first
	if _, err := io.ReadFull(stream, id[1:]); err != nil || !a.beginRequest() {
		stream.CancelWrite(1)
		return
	}
	defer a.endRequest()
	_ = stream.SetDeadline(time.Now().Add(protocol.DirectShareTimeout))
	if err := a.receiveDirectShare(d, string(id[:]), stream); err != nil {
		stream.CancelWrite(1)
		return
	}
	if _, err := stream.Write([]byte{protocol.DirectShareOK}); err != nil {
		stream.CancelWrite(1)
		return
	}
	_ = stream.Close()
}

func (a *Agent) receiveDirectShare(d *directSession, id string, source io.Reader) (err error) {
	a.attachmentMu.Lock()
	upload := a.uploads[id]
	if d.ctx.Err() != nil || upload == nil || upload.Owner != d.client || upload.DirectID != d.id || !upload.Streamed || upload.Offset != -1 || upload.Receiving {
		a.attachmentMu.Unlock()
		return errors.New("unknown direct desktop upload")
	}
	upload.Receiving = true
	if stream, ok := source.(interface{ CancelRead(quic.StreamErrorCode) }); ok {
		upload.AbortReceive = func() { stream.CancelRead(1) }
	}
	a.attachmentMu.Unlock()
	defer func() {
		a.attachmentMu.Lock()
		defer a.attachmentMu.Unlock()
		upload.Receiving = false
		upload.AbortReceive = nil
		if upload.Aborted && err == nil {
			err = errors.New("desktop upload cancelled")
		}
		if err != nil || d.ctx.Err() != nil {
			a.removeDirectShare(id, upload)
			return
		}
		upload.Offset = upload.File.Size
		upload.Updated = time.Now()
	}()
	file, err := os.OpenFile(upload.File.Path+".part", os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := p2p.ReceiveFilePayload(file, source, upload.File.Size); err != nil {
		return err
	}
	return d.ctx.Err()
}

// The attachment mutex must be held by the caller.
func (a *Agent) removeDirectShare(id string, upload *attachmentUpload) {
	_ = os.Remove(upload.File.Path + ".part")
	_ = os.Remove(filepath.Dir(upload.File.Path))
	delete(a.uploads, id)
}

func (a *Agent) clearDirectShares(d *directSession) {
	a.attachmentMu.Lock()
	defer a.attachmentMu.Unlock()
	for id, upload := range a.uploads {
		if upload.Owner == d.client && upload.DirectID == d.id && !upload.Receiving {
			a.removeDirectShare(id, upload)
		}
	}
}
