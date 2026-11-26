package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/google/uuid"
)

const attachmentMaxSize = 20 << 20
const attachmentChunkSize = 256 << 10
const desktopShareMaxSize = protocol.DirectFileMaxSize

type attachment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
}

type attachmentUpload struct {
	File         attachment
	Owner        string
	Offset       int64
	Updated      time.Time
	Downloads    string
	Sender       string
	DirectID     string
	Streamed     bool
	Receiving    bool
	ChatImage    bool
	TransferID   string
	BatchID      string
	AbortReceive func()
	Aborted      bool
}

func attachmentName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
	}
	for len(name) > 180 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		name = "attachment"
	}
	return name
}

func attachmentNumber(params map[string]any, key string, limit int64) (int64, error) {
	v, ok := params[key].(float64)
	if !ok || v < 0 || v > float64(limit) || v != float64(int64(v)) {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return int64(v), nil
}

// Attachment RPCs use small frames on the existing authenticated relay. Files
// stay on the Agent computer so Codex can read them after a browser disconnect.
func removeAbandonedParts(root string, cutoff time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if id, err := uuid.Parse(entry.Name()); err != nil || id.String() != entry.Name() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if !file.Type().IsRegular() || !strings.HasPrefix(file.Name(), "file-") || !strings.HasSuffix(file.Name(), ".part") {
				continue
			}
			if info, err := file.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(dir, file.Name()))
			}
		}
		_ = os.Remove(dir) // Never remove completed attachments or unrelated data.
	}
}

func chatImageMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func (a *Agent) attachmentRPC(owner, method string, params map[string]any, direct *directSession) (any, error) {
	chatImage := strings.HasPrefix(method, protocol.ChatImagePrefix)
	if chatImage {
		operation := strings.TrimPrefix(method, protocol.ChatImagePrefix)
		switch operation {
		case "begin", "chunk", "finish", "abort", "read":
			method = "codexrm/attachment/" + operation
		default:
			return nil, errors.New("unknown chat image operation")
		}
		if params["destination"] != nil || params["uploadStream"] != nil {
			return nil, errors.New("chat image transfer cannot target desktop downloads or use a direct stream")
		}
	}
	a.attachmentMu.Lock()
	defer a.attachmentMu.Unlock()
	root, err := filepath.Abs(filepath.Join(a.cfg.DataDir, "attachments"))
	if err != nil {
		return nil, err
	}
	if a.uploads == nil {
		a.uploads = map[string]*attachmentUpload{}
		cutoff := time.Now().Add(-24 * time.Hour)
		removeAbandonedParts(root, cutoff)
		if downloads, err := desktop.Downloads(); err == nil {
			removeAbandonedParts(filepath.Join(downloads, ".codexrm-incoming"), cutoff)
		}
	}
	for id, upload := range a.uploads {
		if !upload.Receiving && time.Since(upload.Updated) > 30*time.Minute {
			_ = os.Remove(upload.File.Path + ".part")
			_ = os.Remove(filepath.Dir(upload.File.Path))
			delete(a.uploads, id)
		}
	}
	if method == "codexrm/attachment/begin" {
		name, _ := params["name"].(string)
		downloads, sender, transferID, batchID := "", "", "", ""
		limit := int64(attachmentMaxSize)
		if params["destination"] == protocol.DesktopShareDestination {
			if !a.cfg.AllowFileManagement {
				return nil, errFileManagementDisabled
			}
			if direct == nil || direct.ctx.Err() != nil {
				return nil, errors.New(protocol.DesktopShareDirectRequired)
			}
			if batchID, _ = params["batchId"].(string); batchID != "" && !canonicalTransferID(batchID) {
				return nil, errors.New("无效的文件批次 ID")
			}
			if rawID, _ := params["transferId"].(string); rawID != "" {
				if !canonicalTransferID(rawID) {
					return nil, errors.New("无效的发送记录 ID")
				}
				transferID = rawID
				raw, err := os.ReadFile(a.phoneReceipt(owner, transferID))
				if err == nil {
					var receipt attachment
					if err := json.Unmarshal(raw, &receipt); err != nil {
						return nil, err
					}
					return map[string]any{"alreadySaved": true, "name": receipt.Name, "size": receipt.Size}, nil
				}
				if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
				for _, pending := range a.uploads {
					if pending.Owner == owner && pending.TransferID == transferID {
						return nil, errors.New("文件仍在发送，请稍后重试")
					}
				}
			}

			downloads, err = desktop.Downloads()
			if err != nil {
				return nil, err
			}
			limit = desktopShareMaxSize
			sender, _ = params["sender"].(string)
			sender = strings.TrimSpace(strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				return r
			}, sender))
			if runes := []rune(sender); len(runes) > 80 {
				sender = string(runes[:80])
			}
			if sender == "" {
				sender = "已配对设备"
			}
			root = filepath.Join(downloads, ".codexrm-incoming")
		}
		size, err := attachmentNumber(params, "size", limit)
		if err != nil {
			return nil, fmt.Errorf("单个文件最大 %d MB", limit>>20)
		}
		if chatImage && size == 0 {
			return nil, errors.New("图片内容为空")
		}
		if len(a.uploads) >= 30 {
			return nil, fmt.Errorf("同时上传的附件过多，请稍后重试")
		}
		file := attachment{ID: uuid.NewString(), Name: attachmentName(name), Size: size}
		file.Path = filepath.Join(root, file.ID, "file-"+file.Name)
		if err := os.MkdirAll(filepath.Dir(file.Path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(file.Path+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		upload := &attachmentUpload{File: file, Owner: owner, Updated: time.Now(), Downloads: downloads, Sender: sender, ChatImage: chatImage, TransferID: transferID, BatchID: batchID}
		if downloads != "" {
			upload.DirectID = direct.id
			upload.Streamed, _ = params["uploadStream"].(bool)
			if upload.Streamed {
				upload.Offset = -1 // Even an empty file must complete the stream.
			}
		}
		if batchID != "" {
			a.receiveBatches.touch(receiveBatchKey{owner, batchID}, receiveBatchIdle, desktop.ShowReceivedFiles)
		}
		a.uploads[file.ID] = upload
		return map[string]any{"id": file.ID, "chunkSize": attachmentChunkSize, "uploadStream": upload.Streamed}, nil
	}
	id, _ := params["id"].(string)
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return nil, fmt.Errorf("invalid attachment id")
	}
	if method == "codexrm/attachment/read" {
		// Reads only address finalized uploads, never an arbitrary local path.
		raw, err := os.ReadFile(filepath.Join(root, id, "metadata.json"))
		if err != nil {
			return nil, fmt.Errorf("附件不存在或已被移除")
		}
		var file attachment
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, err
		}
		path := filepath.Join(root, id, "file-"+attachmentName(file.Name))
		if file.ID != id || file.Path != path {
			return nil, fmt.Errorf("invalid attachment metadata")
		}
		if chatImage && (!chatImageMIME(file.MimeType) || file.Size <= 0 || file.Size > attachmentMaxSize) {
			return nil, errors.New("中继仅读取已上传的聊天图片")
		}
		offset, err := attachmentNumber(params, "offset", file.Size)
		if err != nil || offset > file.Size {
			return nil, fmt.Errorf("invalid attachment offset")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, attachmentChunkSize))
		if err != nil {
			return nil, err
		}
		if len(data) == 0 && offset < file.Size {
			return nil, fmt.Errorf("附件内容不完整")
		}
		return map[string]any{"file": file, "data": base64.StdEncoding.EncodeToString(data), "offset": offset + int64(len(data))}, nil
	}
	upload := a.uploads[id]
	if upload == nil || upload.Owner != owner || upload.ChatImage != chatImage {
		return nil, fmt.Errorf("上传已失效，请重新选择附件")
	}
	if upload.DirectID != "" && (direct == nil || direct.id != upload.DirectID || direct.ctx.Err() != nil) {
		return nil, errors.New(protocol.DesktopShareDirectRequired)
	}
	if upload.Receiving {
		if method == "codexrm/attachment/abort" && upload.AbortReceive != nil {
			upload.Aborted = true
			upload.AbortReceive()
			return map[string]any{}, nil
		}
		return nil, errors.New("desktop file is still transferring")
	}
	upload.Updated = time.Now()
	switch method {
	case "codexrm/attachment/abort":
		if err := os.Remove(upload.File.Path + ".part"); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		_ = os.Remove(filepath.Dir(upload.File.Path))
		delete(a.uploads, id)
		return map[string]any{}, nil
	case "codexrm/attachment/chunk":
		if upload.Streamed {
			return nil, errors.New("desktop file requires a direct stream")
		}
		offset, err := attachmentNumber(params, "offset", upload.File.Size)
		if err != nil || offset != upload.Offset {
			return nil, fmt.Errorf("附件上传位置不匹配，请重试")
		}
		encoded, _ := params["data"].(string)
		if len(encoded) > base64.StdEncoding.EncodedLen(attachmentChunkSize) {
			return nil, fmt.Errorf("attachment chunk is too large")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 || len(data) > attachmentChunkSize || upload.Offset+int64(len(data)) > upload.File.Size {
			return nil, fmt.Errorf("invalid attachment chunk")
		}
		f, err := os.OpenFile(upload.File.Path+".part", os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		n, writeErr := f.WriteAt(data, upload.Offset)
		closeErr := f.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		upload.Offset += int64(n)
		return map[string]any{"offset": upload.Offset}, nil
	case "codexrm/attachment/finish":
		if upload.Offset != upload.File.Size {
			return nil, fmt.Errorf("附件尚未上传完成")
		}
		if upload.Downloads != "" {
			file, err := finishDesktopShare(upload)
			if err != nil {
				return nil, err
			}
			delete(a.uploads, id)
			if err := a.recordReceivedPhoneFile(file, upload.Sender); err != nil {
				log.Printf("保存文件接收记录: %v", err)
			}
			if upload.TransferID != "" {
				if err := a.savePhoneReceipt(owner, upload.TransferID, file); err != nil {
					return nil, fmt.Errorf("文件仍在发送，请稍后重试????%w", err)
				}
			}

			a.receiveBatches.record(receiveBatchKey{owner, upload.BatchID}, upload.Sender,
				desktop.ReceivedFile{Name: file.Name, Path: file.Path, Size: file.Size}, receiveBatchIdle, desktop.ShowReceivedFiles)
			file.Path = ""
			return file, nil
		}
		f, err := os.Open(upload.File.Path + ".part")
		if err != nil {
			return nil, err
		}
		header := make([]byte, 512)
		n, readErr := f.Read(header)
		_ = f.Close()
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		upload.File.MimeType = strings.Split(http.DetectContentType(header[:n]), ";")[0]
		if chatImage && !chatImageMIME(upload.File.MimeType) {
			_ = os.Remove(upload.File.Path + ".part")
			_ = os.Remove(filepath.Dir(upload.File.Path))
			delete(a.uploads, id)
			return nil, errors.New("中继仅支持 PNG、JPEG、GIF 或 WebP 聊天图片")
		}
		if err := os.Rename(upload.File.Path+".part", upload.File.Path); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(upload.File)
		if err == nil {
			err = os.WriteFile(filepath.Join(filepath.Dir(upload.File.Path), "metadata.json"), raw, 0600)
		}
		if err != nil {
			_ = os.Remove(upload.File.Path)
			delete(a.uploads, id)
			return nil, err
		}
		delete(a.uploads, id)
		return upload.File, nil
	default:
		return nil, fmt.Errorf("unknown attachment method: %s", method)
	}
}

func finishDesktopShare(upload *attachmentUpload) (attachment, error) {
	file := upload.File
	name := file.Name
	// Windows treats these base names as devices even when an extension follows.
	base := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		((strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && strings.Contains("1 2 3 4 5 6 7 8 9 ¹ ² ³", base[3:]) && len([]rune(base[3:])) == 1) {
		name = "_" + name
	}
	ext := filepath.Ext(name)
	for index := 0; index < 10_000; index++ {
		candidate := name
		if index > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), index, ext)
		}
		path := filepath.Join(upload.Downloads, candidate)
		err := desktop.MoveReceivedFile(upload.File.Path+".part", path)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return attachment{}, err
		}
		_ = os.Remove(filepath.Dir(upload.File.Path))
		file.Name, file.Path = candidate, path
		return file, nil
	}
	return attachment{}, fmt.Errorf("下载文件夹中同名文件过多")
}
