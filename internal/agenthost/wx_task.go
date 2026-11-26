package agenthost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type wxTaskStep struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type wxImageProgress struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type wxTask struct {
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	Action           string            `json:"action,omitempty"`
	Backup           string            `json:"backup,omitempty"`
	Status           string            `json:"status"`
	Step             string            `json:"step,omitempty"`
	Steps            []wxTaskStep      `json:"steps"`
	OptionalServices []string          `json:"optionalServices,omitempty"`
	Images           []wxImageProgress `json:"images,omitempty"`
	Error            string            `json:"error,omitempty"`
	PrivilegeCommand string            `json:"privilegeCommand,omitempty"`
	InstallDir       string            `json:"installDir,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
}

type wxInstance struct {
	Mode            string               `json:"mode"`
	ComposeDir      string               `json:"composeDir,omitempty"`
	TakenOverAt     time.Time            `json:"takenOverAt,omitempty"`
	InstalledAt     time.Time            `json:"installedAt,omitempty"`
	AdminVerified   bool                 `json:"adminVerified,omitempty"`
	Snapshot        *wxExisting          `json:"snapshot,omitempty"`
	LastContactSync map[string]time.Time `json:"lastContactSync,omitempty"`
}

type wxTaskStore struct {
	mu          sync.Mutex
	running     map[string]bool
	pendingSave map[string]*wxTask
}

var wxRuntime = wxTaskStore{running: map[string]bool{}, pendingSave: map[string]*wxTask{}}
var wxTakeoverTokens sync.Map

func (h *Host) wxTaskPath() string {
	return filepath.Join(h.StateDir(), "wx-task.json")
}

func (h *Host) wxInstancePath() string {
	return filepath.Join(h.StateDir(), "wx-instance.json")
}

func readJSONFile[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wx-state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (h *Host) loadWXTask() *wxTask {
	task, _ := readJSONFile[wxTask](h.wxTaskPath())
	return task
}

func (h *Host) saveWXTask(task *wxTask) error {
	if task == nil {
		return nil
	}
	task.UpdatedAt = time.Now().UTC()
	return writeJSONFile(h.wxTaskPath(), task)
}

func (h *Host) loadWXInstance() *wxInstance {
	inst, _ := readJSONFile[wxInstance](h.wxInstancePath())
	return inst
}

func (h *Host) updateWXInstance(change func(*wxInstance) *wxInstance) error {
	h.wxInstanceMu.Lock()
	defer h.wxInstanceMu.Unlock()
	inst, err := readJSONFile[wxInstance](h.wxInstancePath())
	if err != nil {
		return err
	}
	return writeJSONFile(h.wxInstancePath(), change(inst))
}

func (h *Host) saveWXInstance(inst *wxInstance) error {
	return h.updateWXInstance(func(before *wxInstance) *wxInstance {
		if before != nil {
			inst.LastContactSync = before.LastContactSync
		}
		return inst
	})
}

func newWXTaskID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func wxInstallSteps() []wxTaskStep {
	return []wxTaskStep{
		{ID: "check-env", Name: "检查系统环境", Status: "pending"},
		{ID: "install-docker", Name: "安装 Docker 与 Compose", Status: "pending"},
		{ID: "prepare-files", Name: "准备数据目录和配置", Status: "pending"},
		{ID: "pull-images", Name: "拉取容器镜像", Status: "pending"},
		{ID: "start-services", Name: "启动公共服务", Status: "pending"},
		{ID: "wait-healthy", Name: "等待服务就绪", Status: "pending"},
		{ID: "create-robot", Name: "创建机器人实例", Status: "pending"},
	}
}

func wxTakeoverSteps() []wxTaskStep {
	return []wxTaskStep{
		{ID: "detect", Name: "识别已有安装", Status: "pending"},
		{ID: "verify-credentials", Name: "验证管理凭据", Status: "pending"},
		{ID: "snapshot", Name: "记录账号、服务和数据目录", Status: "pending"},
		{ID: "probe-capabilities", Name: "探测客户端能力", Status: "pending"},
	}
}

func setWXStep(task *wxTask, id, status, errText string) {
	task.Step = id
	task.Status = "running"
	if status == "failed" {
		task.Status = "failed"
		task.Error = errText
	}
	if status == "needs_privilege" {
		task.Status = "needs_privilege"
		task.Error = errText
	}
	for i, step := range task.Steps {
		if step.ID == id {
			task.Steps[i].Status = status
			task.Steps[i].Error = errText
			return
		}
	}
}

func (h *Host) beginWXTask(kind string, steps []wxTaskStep, dir string, configure ...func(*wxTask)) (*wxTask, error) {
	wxRuntime.mu.Lock()
	defer wxRuntime.mu.Unlock()
	current, err := readJSONFile[wxTask](h.wxTaskPath())
	if err != nil {
		return nil, err
	}
	if wxRuntime.running[h.StateDir()] || wxRuntime.pendingSave[h.StateDir()] != nil || current != nil && (current.Status == "running" || current.Status == "pending") {
		return nil, errors.New("已有安装或接管任务正在进行")
	}
	task := &wxTask{
		ID:         newWXTaskID(),
		Kind:       kind,
		Status:     "pending",
		Steps:      steps,
		InstallDir: dir,
		CreatedAt:  time.Now().UTC(),
	}
	for _, set := range configure {
		set(task)
	}
	if err := h.saveWXTask(task); err != nil {
		wxTakeoverTokens.Delete(task.ID)
		return nil, err
	}
	return task, nil
}

func (h *Host) startWXTask(task *wxTask, run func(context.Context, *wxTask) error) {
	wxRuntime.mu.Lock()
	defer wxRuntime.mu.Unlock()
	dir := h.StateDir()
	h.mu.Lock()
	parent := h.ctx
	h.mu.Unlock()
	latest := h.loadWXTask()
	if parent == nil || parent.Err() != nil || wxRuntime.running[dir] || wxRuntime.pendingSave[dir] != nil || latest == nil || latest.ID != task.ID || latest.Status != "pending" {
		return
	}
	latest.Status = "running"
	if err := h.saveWXTask(latest); err != nil {
		log.Printf("start WeChat task: %v", err)
		return
	}
	wxRuntime.running[dir] = true
	h.wxTaskWG.Add(1)
	go func() {
		defer h.wxTaskWG.Done()
		ctx, cancel := context.WithTimeout(parent, 45*time.Minute)
		defer cancel()
		err := run(ctx, latest)
		wxRuntime.mu.Lock()
		defer wxRuntime.mu.Unlock()
		defer delete(wxRuntime.running, dir)
		if err != nil && latest.Status == "running" {
			if ctx.Err() != nil {
				latest.Status = "interrupted"
				latest.Error = err.Error()
			} else {
				setWXStep(latest, latest.Step, "failed", err.Error())
			}
		} else if err == nil && latest.Status == "running" {
			latest.Status = "completed"
			latest.Error = ""
			latest.Step = ""
		}
		if err := h.saveWXTask(latest); err != nil {
			wxRuntime.pendingSave[dir] = latest
			log.Printf("save WeChat task result: %v", err)
		}
	}()
}

func (h *Host) resumeWXTask() {
	wxRuntime.mu.Lock()
	dir := h.StateDir()
	if wxRuntime.running[dir] {
		wxRuntime.mu.Unlock()
		return
	}
	if result := wxRuntime.pendingSave[dir]; result != nil {
		if err := h.saveWXTask(result); err == nil {
			delete(wxRuntime.pendingSave, dir)
		}
		wxRuntime.mu.Unlock()
		return
	}
	task := h.loadWXTask()
	if task != nil && task.Status == "running" {
		task.Status = "interrupted"
		task.Error = "上次运行已中断，请检查服务状态后再发起操作"
		if err := h.saveWXTask(task); err != nil {
			wxRuntime.pendingSave[dir] = task
		}
	}
	wxRuntime.mu.Unlock()
	if task == nil || task.Status != "pending" {
		return
	}
	switch task.Kind {
	case "install":
		h.startWXTask(task, h.runWXInstall)
	case "takeover":
		h.startWXTask(task, h.runWXTakeover)
	case "maintenance":
		h.startWXTask(task, h.runWXMaintenance)
	}
}

func (h *Host) wxTask(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.resumeWXTask()
		wxRuntime.mu.Lock()
		task := h.loadWXTask()
		if wxRuntime.pendingSave[h.StateDir()] != nil {
			task = wxRuntime.pendingSave[h.StateDir()]
			copy := *task
			copy.Status = "interrupted"
			copy.Error = "任务结果暂时无法保存，请检查磁盘和文件权限"
			task = &copy
		}
		wxRuntime.mu.Unlock()
		respond(w, 200, task)
	case http.MethodPost:
		var input struct {
			Retry bool `json:"retry"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			failure(w, err)
			return
		}
		if !input.Retry {
			failure(w, errors.New("不支持的任务操作"))
			return
		}
		wxRuntime.mu.Lock()
		task := h.loadWXTask()
		if wxRuntime.running[h.StateDir()] || task == nil || (task.Status != "failed" && task.Status != "needs_privilege") {
			wxRuntime.mu.Unlock()
			failure(w, errors.New("没有可重试的任务"))
			return
		}
		task.Status = "pending"
		task.Error = ""
		task.PrivilegeCommand = ""
		for i, step := range task.Steps {
			if step.Status == "failed" || step.Status == "needs_privilege" || step.Status == "running" {
				task.Steps[i].Status = "pending"
				task.Steps[i].Error = ""
			}
		}
		err := h.saveWXTask(task)
		wxRuntime.mu.Unlock()
		if err != nil {
			failure(w, err)
			return
		}
		h.resumeWXTask()
		respond(w, 200, task)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
