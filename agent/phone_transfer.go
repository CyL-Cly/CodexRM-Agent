package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/google/uuid"
)

const phoneQueueLimit = 64
const phoneQueueBytes int64 = 2 << 30

type PhoneTransfer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Created   int64  `json:"created"`
	Recipient string `json:"recipient,omitempty"`
}

// Only serializes opening and removing outbox files. The direct file reader
// itself already serializes chunk reads with closing the handle.
type phoneFileReaders struct {
	mu    sync.Mutex
	paths map[*directSession]string
}

func (a *Agent) closePhoneReader(d *directSession) {
	a.phoneReads.mu.Lock()
	defer a.phoneReads.mu.Unlock()
	d.phoneFiles.close()
	delete(a.phoneReads.paths, d)
}

func phoneOwnerBucket(owner string) string {
	digest := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(digest[:])
}

func (a *Agent) phoneTransferRoot(kind string) string {
	return filepath.Join(a.cfg.DataDir, "phone-transfer", kind)
}

func (a *Agent) phoneTransferDir(owner, kind string) string {
	return filepath.Join(a.phoneTransferRoot(kind), phoneOwnerBucket(owner))
}

func canonicalTransferID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

// QueuePhoneFile is a local-only entry point. Remote peers can read only items
// explicitly staged for their authenticated client ID; no remote path is accepted.
func (a *Agent) QueuePhoneFile(owner, path, recipient string) (PhoneTransfer, error) {
	var item PhoneTransfer
	if owner == "" || !filepath.IsAbs(path) {
		return item, errors.New("无效的收件手机或文件路径")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return item, err
	}
	if !info.Mode().IsRegular() || info.Size() > directFileMax {
		return item, errors.New("仅支持不超过 512 MB 的普通文件")
	}
	source, err := os.Open(path)
	if err != nil {
		return item, err
	}
	defer source.Close()
	if actual, err := source.Stat(); err != nil || !os.SameFile(info, actual) {
		return item, errors.New("文件已变更，请重试")
	}
	base := a.phoneTransferDir(owner, "outbox")
	if err = os.MkdirAll(base, 0700); err != nil {
		return item, err
	}
	// Serialize staging (including across multiple Explorer invocations).
	lock := filepath.Join(base, ".staging-lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return item, err
		}
		// A large local copy can take a while. Reclaim only a clearly abandoned lock.
		if stamp, e := os.Stat(lock); e != nil || time.Since(stamp.ModTime()) < 24*time.Hour {
			return item, errors.New("另一个文件正在准备，请稍后重试")
		}
		if err = os.Remove(lock); err != nil {
			return item, err
		}
		if err = os.Mkdir(lock, 0700); err != nil {
			return item, err
		}
	}
	defer os.Remove(lock)
	entries, err := listPhoneTransfers(base)
	if err != nil {
		return item, err
	}
	total := info.Size()
	for _, entry := range entries {
		total += entry.Size
	}
	if len(entries) >= phoneQueueLimit || total > phoneQueueBytes {
		return item, errors.New("该手机的待发送缓存已满，请先在手机上接收")
	}
	stage, err := os.MkdirTemp(base, ".staging-")
	if err != nil {
		return item, err
	}
	defer os.RemoveAll(stage)
	f, err := os.OpenFile(filepath.Join(stage, "data"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return item, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(source, info.Size()+1))
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return item, err
	}
	if actual, err := source.Stat(); err != nil || n != info.Size() || actual.Size() != n || !actual.ModTime().Equal(info.ModTime()) {
		return item, errors.New("文件已变更，请重试")
	}
	item = PhoneTransfer{ID: uuid.NewString(), Name: attachmentName(filepath.Base(path)), Size: n, Created: time.Now().UnixMilli(), Recipient: recipient}
	raw, _ := json.Marshal(item)
	if err := os.WriteFile(filepath.Join(stage, "info.json"), raw, 0600); err != nil {
		return item, err
	}
	return item, os.Rename(stage, filepath.Join(base, item.ID))
}

func loadPhoneTransfer(base, id string) (PhoneTransfer, error) {
	var item PhoneTransfer
	if !canonicalTransferID(id) {
		return item, errors.New("无效的文件 ID")
	}
	raw, err := os.ReadFile(filepath.Join(base, id, "info.json"))
	if err != nil {
		return item, err
	}
	if json.Unmarshal(raw, &item) != nil || item.ID != id || item.Name == "" || item.Size < 0 || item.Size > directFileMax {
		return item, errors.New("文件缓存元数据无效")
	}
	return item, nil
}

func listPhoneTransfers(base string) ([]PhoneTransfer, error) {
	entries, err := os.ReadDir(base)
	list := []PhoneTransfer{}
	if errors.Is(err, os.ErrNotExist) {
		return list, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if item, err := loadPhoneTransfer(base, entry.Name()); err == nil {
				list = append(list, item)
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Created == list[j].Created {
			return list[i].ID < list[j].ID
		}
		return list[i].Created < list[j].Created
	})
	return list, nil
}

func (a *Agent) phoneTransferRPC(ctx context.Context, owner, method string, params map[string]any, d *directSession) (any, error) {
	if owner == "" {
		return nil, errors.New("需要已配对的手机")
	}
	base := a.phoneTransferDir(owner, "outbox")
	switch method {
	case protocol.PhoneTransferPrefix + "list":
		list, err := listPhoneTransfers(base)
		return map[string]any{"files": list}, err
	}
	if d == nil || d.client != owner || d.ctx.Err() != nil {
		return nil, errors.New(protocol.FileTransferDirectRequired)
	}
	id, _ := params["id"].(string)
	switch method {
	case protocol.PhoneTransferPrefix + "open":
		a.phoneReads.mu.Lock()
		defer a.phoneReads.mu.Unlock()
		item, err := loadPhoneTransfer(base, id)
		if err != nil {
			return nil, errors.New("待接收文件不存在")
		}
		path := filepath.Join(base, id)
		f, err := os.Open(filepath.Join(path, "data"))
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != item.Size {
			f.Close()
			return nil, errors.New("文件缓存已损坏")
		}
		streamed, _ := params["downloadStream"].(bool)
		result, err := d.phoneFiles.openRead(ctx, f, info, item.Name, streamed)
		if err == nil {
			if a.phoneReads.paths == nil {
				a.phoneReads.paths = make(map[*directSession]string)
			}
			a.phoneReads.paths[d] = path
		}
		return result, err
	case protocol.PhoneTransferPrefix + "read", protocol.PhoneTransferPrefix + "close":
		return d.phoneFiles.readChunk(id, params["offset"], method == protocol.PhoneTransferPrefix+"close")
	case protocol.PhoneTransferPrefix + "finishBatch":
		if !canonicalTransferID(id) {
			return nil, errors.New("无效的文件批次 ID")
		}
		a.receiveBatches.flush(receiveBatchKey{owner, id}, desktop.ShowReceivedFiles)
		return map[string]any{}, nil
	case protocol.PhoneTransferPrefix + "ack":
		if !canonicalTransferID(id) {
			return nil, errors.New("无效的文件 ID")
		}
		// Idempotent: a lost ACK reply must not prevent the next sync.
		return map[string]any{}, a.removePhoneTransfer(base, id, false)
	default:
		return nil, errors.New("未知的文件传输操作")
	}
}

// Receipts let the phone safely retry after a lost finish response. The
// existing Downloads destination is retained and no local path is returned.
func (a *Agent) phoneReceipt(owner, id string) string {
	return filepath.Join(a.phoneTransferDir(owner, "receipts"), id+".json")
}

func (a *Agent) savePhoneReceipt(owner, id string, file attachment) error {
	dir := a.phoneTransferDir(owner, "receipts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Bound local receipt growth to the retry window; the phone keeps history.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !info.IsDir() && time.Since(info.ModTime()) > 90*24*time.Hour {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	file.Path = ""
	raw, _ := json.Marshal(file)
	f, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), a.phoneReceipt(owner, id))
}
