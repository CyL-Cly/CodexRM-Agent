package agent

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
)

var ErrPhoneTransferNotPending = errors.New("该文件已接收或已取消")

// PendingPhoneTransfer identifies a local cache entry even when the relay is
// offline or the recipient has since been unpaired.
type PendingPhoneTransfer struct {
	PhoneTransfer
	Bucket string `json:"bucket"`
}

func validPhoneBucket(bucket string) bool {
	if len(bucket) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(bucket)
	return err == nil && hex.EncodeToString(decoded) == bucket
}

func (a *Agent) PendingPhoneTransfers() ([]PendingPhoneTransfer, error) {
	root := a.phoneTransferRoot("outbox")
	items := []PendingPhoneTransfer{}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validPhoneBucket(entry.Name()) {
			continue
		}
		files, err := listPhoneTransfers(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			items = append(items, PendingPhoneTransfer{file, entry.Name()})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created > items[j].Created
	})
	return items, nil
}

func (a *Agent) CancelPhoneTransfer(bucket, id string) error {
	if !validPhoneBucket(bucket) || !canonicalTransferID(id) {
		return errors.New("无效的发送记录")
	}
	return a.removePhoneTransfer(filepath.Join(a.phoneTransferRoot("outbox"), bucket), id, true)
}

func (a *Agent) removePhoneTransfer(base, id string, mustBePending bool) error {
	a.phoneReads.mu.Lock()
	defer a.phoneReads.mu.Unlock()
	path := filepath.Join(base, id)
	for _, dir := range []string{base, path} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			if mustBePending {
				return ErrPhoneTransferNotPending
			}
			return nil // A repeated ACK is still accepted.
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("发送缓存目录无效")
		}
	}
	if mustBePending {
		if _, err := loadPhoneTransfer(base, id); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrPhoneTransferNotPending
			}
			return err
		}
	}
	for session, opened := range a.phoneReads.paths {
		if opened == path {
			session.phoneFiles.close()
			delete(a.phoneReads.paths, session)
		}
	}
	// Only the two known cache files are removed, never the original selected
	// file or any unrelated contents of the state directory.
	for _, name := range []string{"data", "info.json", ""} {
		if err := os.Remove(filepath.Join(path, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

const phoneReceiveHistoryLimit = 200

type ReceivedPhoneFile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Sender  string `json:"sender"`
	Size    int64  `json:"size"`
	Created int64  `json:"created"`
}

type receivedPhoneRecord struct {
	ReceivedPhoneFile
	Identity string `json:"fileIdentity"`
}

func validReceivedPhoneName(name string) bool {
	return filepath.IsLocal(name) && filepath.Base(name) == name && !strings.ContainsAny(name, "\\/:")
}

func (a *Agent) recordReceivedPhoneFile(file attachment, sender string) error {
	if !canonicalTransferID(file.ID) || !validReceivedPhoneName(file.Name) {
		return errors.New("无效的接收记录")
	}
	identity, err := desktop.ReceivedFileIdentity(file.Path)
	if err != nil {
		return err
	}
	dir := a.phoneTransferRoot("inbox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	item := ReceivedPhoneFile{ID: file.ID, Name: file.Name, Sender: sender, Size: file.Size, Created: time.Now().UnixMilli()}
	raw, err := json.Marshal(receivedPhoneRecord{ReceivedPhoneFile: item, Identity: identity})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".received-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, file.ID+".json")); err != nil {
		return err
	}
	items, err := a.receivedPhoneFiles()
	if err != nil {
		return err
	}
	for _, old := range items[min(len(items), phoneReceiveHistoryLimit):] {
		_ = os.Remove(filepath.Join(dir, old.ID+".json"))
	}
	return nil
}

func (a *Agent) loadReceivedPhoneFile(id string) (receivedPhoneRecord, error) {
	var item receivedPhoneRecord
	if !canonicalTransferID(id) {
		return item, errors.New("无效的接收记录")
	}
	raw, err := os.ReadFile(filepath.Join(a.phoneTransferRoot("inbox"), id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return item, errors.New("接收记录不存在")
	}
	if err != nil {
		return item, err
	}
	if json.Unmarshal(raw, &item) != nil || item.ID != id || !validReceivedPhoneName(item.Name) || item.Size < 0 || item.Identity == "" {
		return item, errors.New("接收记录无效")
	}
	return item, nil
}

func (a *Agent) receivedPhoneFiles() ([]ReceivedPhoneFile, error) {
	dir := a.phoneTransferRoot("inbox")
	items := []ReceivedPhoneFile{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		if item, err := a.loadReceivedPhoneFile(id); err == nil {
			items = append(items, item.ReceivedPhoneFile)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created > items[j].Created
	})
	return items, nil
}

func (a *Agent) ReceivedPhoneFiles() ([]ReceivedPhoneFile, error) {
	items, err := a.receivedPhoneFiles()
	return items[:min(len(items), phoneReceiveHistoryLimit)], err
}

func (a *Agent) OpenReceivedPhoneFile(id string) error {
	downloads, err := desktop.Downloads()
	if err != nil {
		return err
	}
	path, err := a.receivedPhonePath(downloads, id)
	if err != nil {
		return err
	}
	return desktop.OpenReceivedFile(path)
}

func (a *Agent) receivedPhonePath(downloads, id string) (string, error) {
	item, err := a.loadReceivedPhoneFile(id)
	if err != nil {
		return "", err
	}
	path := filepath.Join(downloads, item.Name)
	identity, err := desktop.ReceivedFileIdentity(path)
	if err != nil {
		return "", err
	}
	if identity != item.Identity {
		return "", errors.New("下载文件夹中的同名文件已被替换")
	}
	return path, nil
}
