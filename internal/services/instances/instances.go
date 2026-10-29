// Package instances 多实例管理（2026-10-14 实例管理页 V1 后端）。
//
// 形态：同机跑多套「中控 + 机器人 + data-dir + deploy-dir + 端口对」栈，
// 本包负责：①实例注册表（<exe目录>/instances.json，所有实例共享一份）；
// ②进程定位（**只按命令行/可执行文件路径匹配**，绝不按镜像名盲杀）；
// ③生命周期操作（start/stop/restart/delete）与创建向导（预检+目录复制+脚本生成）。
//
// 红线（与 internal/services/process 同口径）：
//   - 绝不 taskkill /IM：同机所有实例的中控镜像名相同（zyctrlcenter.exe），
//     机器人镜像名也相同（robot_single_robot.exe），按镜像名杀会连带别的栈；
//   - 定位失败/输出为空 → 视为 false（宁可不动作）。
package instances

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Instance 一个实例（= 一套栈）的注册信息。JSON 字段名与前端契约对齐，不可改。
type Instance struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ZoneServer  string `json:"zone_server"` // 服 key（zones.json 的 server.key，如 "prod-240"）
	ZoneKey     string `json:"zone_key"`    // 区 key（如 "2400"）
	WebPort     int    `json:"web_port"`    // 中控 HTTP API 端口
	CtrlPort    int    `json:"ctrl_port"`   // 机器人控制通道端口（机器人 ROBOT_CTRL_PORT 连它）
	BaseDir     string `json:"base_dir,omitempty"`
	DataDir     string `json:"data_dir"`
	DeployDir   string `json:"deploy_dir"`
	StartScript string `json:"start_script"`          // 启动脚本文件名（相对 exe 目录；如 "_start_prod-240_2300.bat"）
	WarmScript  string `json:"warm_script,omitempty"` // 温启脚本（仅重启中控、不动机器人；restart controller 优先用；2026-10-14 P1-1）
	Stopped     bool   `json:"stopped,omitempty"`     // 人工停止标记：true=面板人工停止，看门狗不得自动拉起（点「启动」清除；2026-10-14）
	CtrlMatch   string `json:"ctrl_match"`            // 中控进程命令行匹配串（唯一允许的定位方式）
	Note        string `json:"note,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// IsSelf 该实例是否为本进程自身（web_port 相同）。自身实例禁止 stop/controller/cold 重启/删除。
func (i Instance) IsSelf(webPort int) bool { return i.WebPort > 0 && i.WebPort == webPort }

type fileFormat struct {
	Instances []Instance `json:"instances"`
}

// Registry 实例注册表（并发安全；原子落盘）。
type Registry struct {
	path string

	mu        sync.Mutex
	instances []Instance
}

// NewRegistry 创建注册表；path 通常为 <exe目录>/instances.json。
func NewRegistry(path string) *Registry { return &Registry{path: path} }

// Path 注册表文件路径。
func (r *Registry) Path() string { return r.path }

// Load 加载注册表。
//
//   - 文件不存在：以 seed 写入一条并落盘（"本进程自身"种子），返回 seeded=true；
//   - 文件存在：解析 + 规整（缺失字段补全：id 缺省 "p<web_port>"、
//     ctrl_match 缺省按 data_dir 派生 `--data-dir "<data_dir>"`），有补全则回写。
//
// 解析失败返回错误（调用方应视为"实例管理不可用"，宁可不动作）。
func (r *Registry) Load(seed Instance) (seeded bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	raw, err := os.ReadFile(r.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		n := normalize(seed)
		if n.ID == "" {
			return false, nil // 无种子（seed 也没有 web_port）：保持空表
		}
		r.instances = []Instance{n}
		return true, r.saveLocked()
	}

	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return false, fmt.Errorf("实例注册表 %s 解析失败: %w", r.path, err)
	}
	merged := false
	out := make([]Instance, 0, len(f.Instances))
	for _, inst := range f.Instances {
		n := normalize(inst)
		if n != inst {
			merged = true
		}
		out = append(out, n)
	}
	r.instances = out
	if merged {
		if err := r.saveLocked(); err != nil {
			return false, err // 内存已加载；回写失败仅提示上抛
		}
	}
	return false, nil
}

// normalize 规整单条实例（合并补全缺失字段；纯函数便于单测）。
func normalize(inst Instance) Instance {
	inst.ID = strings.TrimSpace(inst.ID)
	inst.Name = strings.TrimSpace(inst.Name)
	inst.ZoneServer = strings.TrimSpace(inst.ZoneServer)
	inst.ZoneKey = strings.TrimSpace(inst.ZoneKey)
	inst.BaseDir = strings.TrimSpace(inst.BaseDir)
	inst.DataDir = strings.TrimSpace(inst.DataDir)
	inst.DeployDir = strings.TrimSpace(inst.DeployDir)
	inst.StartScript = strings.TrimSpace(inst.StartScript)
	inst.CtrlMatch = strings.TrimSpace(inst.CtrlMatch)
	inst.Note = strings.TrimSpace(inst.Note)
	if inst.ID == "" && inst.WebPort > 0 {
		inst.ID = fmt.Sprintf("p%d", inst.WebPort)
	}
	// ctrl_match 合并补全：缺省按 data_dir 派生（与手工注册的 B 实例同款）；
	// data_dir 也空时退回 --web-port（注意：仅当启动命令行确含该参数时才有效）。
	if inst.CtrlMatch == "" {
		switch {
		case inst.DataDir != "":
			// 注意：不能用 %q（会转义反斜杠成 \\）——命令行匹配串必须与真实命令行一致。
			inst.CtrlMatch = `--data-dir "` + inst.DataDir + `"`
		case inst.WebPort > 0:
			inst.CtrlMatch = fmt.Sprintf("--web-port %d", inst.WebPort)
		}
	}
	return inst
}

// List 深拷贝快照。
func (r *Registry) List() []Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Instance(nil), r.instances...)
}

// Count 实例数。
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.instances)
}

// Get 按 id 取（不存在返回 false）。
func (r *Registry) Get(id string) (Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range r.instances {
		if inst.ID == id {
			return inst, true
		}
	}
	return Instance{}, false
}

// Add 追加一个实例（id/web_port/ctrl_port 查重），落盘。
func (r *Registry) Add(inst Instance) error {
	inst = normalize(inst)
	if inst.ID == "" {
		return fmt.Errorf("实例 id 不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, old := range r.instances {
		if old.ID == inst.ID {
			return fmt.Errorf("实例 id 已存在: %s", inst.ID)
		}
		if inst.WebPort > 0 && old.WebPort == inst.WebPort {
			return fmt.Errorf("web 端口 %d 已被实例 %s 占用", inst.WebPort, old.ID)
		}
		if inst.CtrlPort > 0 && old.CtrlPort == inst.CtrlPort {
			return fmt.Errorf("ctrl 端口 %d 已被实例 %s 占用", inst.CtrlPort, old.ID)
		}
	}
	r.instances = append(r.instances, inst)
	return r.saveLocked()
}

// Update 按 id 整条替换（用于人工停止标记等字段更新；找不到返回 false），落盘。
func (r *Registry) Update(inst Instance) (bool, error) {
	inst = normalize(inst)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.instances {
		if r.instances[i].ID == inst.ID {
			r.instances[i] = inst
			return true, r.saveLocked()
		}
	}
	return false, nil
}

// Remove 移除一个实例（返回被移除的条目），落盘。
func (r *Registry) Remove(id string) (Instance, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, inst := range r.instances {
		if inst.ID != id {
			continue
		}
		r.instances = append(r.instances[:i], r.instances[i+1:]...)
		return inst, true, r.saveLocked()
	}
	return Instance{}, false, nil
}

func (r *Registry) saveLocked() error {
	raw, err := json.MarshalIndent(fileFormat{Instances: r.instances}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// nowUnix 统一取时刻（秒），便于测试注入。
func nowUnix() int64 { return time.Now().Unix() }
