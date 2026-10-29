// 实例生命周期操作（start / stop / restart / delete）。
//
// 安全约束：
//   - 所有写操作共用一把 mutex（防双请求同实例同时 start/stop/delete/create）；
//   - "自身实例"（web_port == 本进程）拒绝 stop / controller / cold 重启 / delete
//     （防止管理页把自己弄没）；
//   - 杀进程只按 PID（taskkill /PID /F），**绝不 taskkill /IM**。
package instances

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/services/zones"
)

// ErrSelfOperation 拒绝对"本进程自身实例"执行停止/重启/删除。
var ErrSelfOperation = errors.New("不能操作自身实例")

// Options Service 构造参数。
type Options struct {
	ExeDir        string          // exe 目录（instances.json 与启动脚本所在）
	DataDir       string          // 本实例 data（创建向导的模板源）
	WebPort       int             // 本进程 web 端口（"自身"判断）
	WebDistDir    string          // 共享 web/dist（生成启动脚本用）
	GameConfigDir string          // 共享 game-config（生成启动脚本用）
	Zones         *zones.Registry // 主 zones（创建向导校验；可为 nil，退回 SeedServers）
	Logf          func(string, ...any)
}

// Service 实例管理服务（注册表 + 进程定位 + 生命周期操作）。
type Service struct {
	opts Options
	reg  *Registry

	// 可注入依赖（测试用；nil = 真实实现）。
	RunPowerShell  func(script string) ([]byte, error) // PowerShell 执行器（进程定位）
	KillPID        func(pid int) error                 // 杀单进程
	RunStartScript func(workDir, scriptPath string) error
	Sleep          func(d time.Duration)
	Now            func() time.Time
	HTTPClient     *http.Client // 状态探测 / robot 代理（nil = 默认超时）

	// mu 全部写操作共用（start/stop/restart/delete/create 串行）。
	mu sync.Mutex
}

// New 创建服务（注册表路径 = <ExeDir>/instances.json；不含 Load，由调用方显式调用）。
func New(opts Options) *Service {
	return &Service{opts: opts, reg: NewRegistry(filepath.Join(opts.ExeDir, "instances.json"))}
}

// RegistryPath 注册表文件路径。
func (s *Service) RegistryPath() string { return s.reg.Path() }

// List 全部实例（注册字段快照）。
func (s *Service) List() []Instance { return s.reg.List() }

// Count 实例数。
func (s *Service) Count() int { return s.reg.Count() }

// Get 按 id 取实例。
func (s *Service) Get(id string) (Instance, bool) { return s.reg.Get(id) }

// Add 追加一个实例到注册表（id/web_port/ctrl_port 查重）。
func (s *Service) Add(inst Instance) error { return s.reg.Add(inst) }

// Load 加载注册表（不存在则以 seed 种子一条并落盘；返回 seeded）。
func (s *Service) Load(seed Instance) (bool, error) { return s.reg.Load(seed) }

// WebPort 本进程 web 端口（"自身"判断口径）。
func (s *Service) WebPort() int { return s.opts.WebPort }

func (s *Service) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

func (s *Service) sleep(d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------- start

// StartResult start/controller|cold 重启的返回。
type StartResult struct {
	Msg     string
	CtrlPID int
}

// Start 执行实例启动脚本（等 3s 后回报中控 PID；PID=0 表示 3s 内未定位到）。
func (s *Service) Start(id string) (StartResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.reg.Get(id)
	if !ok {
		return StartResult{}, fmt.Errorf("实例不存在: %s", id)
	}
	// 2026-10-14 复核 P2-1：自身实例的 start 无意义（页面能打开说明自身在跑），拒绝。
	if inst.IsSelf(s.opts.WebPort) {
		return StartResult{}, ErrSelfOperation
	}
	// 启动=意图恢复运行：先清人工停止标记（恢复看门狗看护；即使脚本执行失败，
	// 看门狗也能按 start_script 兜底重拉）。
	if err := s.markStopped(inst, false); err != nil {
		s.logf("[INSTANCES] start %s：清除人工停止标记失败: %v", id, err)
	}
	return s.startLocked(inst)
}

func (s *Service) startLocked(inst Instance) (StartResult, error) {
	return s.startLockedWith(inst, inst.StartScript)
}

// startLockedWith 用指定脚本启动（2026-10-14 复核 P1-1：restart controller 可走 warm_script）。
func (s *Service) startLockedWith(inst Instance, script string) (StartResult, error) {
	if err := s.runScript(inst, script); err != nil {
		return StartResult{}, err
	}
	s.sleep(3 * time.Second) // 契约：等 3s（给中控起来）后回 PID
	pid := 0
	if pids := s.FindCtrlPIDs(inst.CtrlMatch); len(pids) > 0 {
		pid = pids[0]
	}
	s.logf("[INSTANCES] start %s（脚本 %s）ctrl_pid=%d", inst.ID, script, pid)
	return StartResult{Msg: "启动脚本已执行: " + script, CtrlPID: pid}, nil
}

// runScript 以 detached 方式执行实例启动/温启脚本（cmd /c start "" /min；工作目录=exe 目录）。
func (s *Service) runScript(inst Instance, script string) error {
	script = strings.TrimSpace(script)
	if script == "" {
		return fmt.Errorf("实例 %s 未配置启动脚本", inst.ID)
	}
	path := script
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.opts.ExeDir, script)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("启动脚本不存在: %s", path)
	}
	if s.RunStartScript != nil {
		return s.RunStartScript(s.opts.ExeDir, path)
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("非 Windows 平台不支持拉起实例")
	}
	return startDetached(s.opts.ExeDir, path)
}

// startDetached cmd /c start "" /min <bat>（独立最小化窗口；父进程退出不带走）。
func startDetached(workDir, scriptPath string) error {
	cmd := exec.Command("cmd", "/c", "start", "", "/min", scriptPath)
	cmd.Dir = workDir
	return cmd.Run()
}

// ---------------------------------------------------------------- stop

// StopResult 停止结果（被杀 PID 列表；Warnings=人工停止标记写入失败等提示）。
type StopResult struct {
	Ctrl     []int    `json:"ctrl"`
	Robot    []int    `json:"robot"`
	Warnings []string `json:"warnings,omitempty"`
}

// Stop 停止实例：按 ctrl_match 定位中控 + 按 deploy_dir 边界定位机器人，逐个 taskkill /PID。
//
// 人工停止语义（2026-10-14）：先写 stopped=true 标记再杀进程——看门狗据此跳过自动拉起，
// 且"杀完到写标记之间"的竞态窗口被消除。标记写失败不阻断停止，但会放进 Warnings 提示。
// 自身实例拒绝（ErrSelfOperation）。
func (s *Service) Stop(id string) (StopResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked(id)
}

// markStopped 更新实例的人工停止标记（值变化才写盘）。调用方需已持有 s.mu。
func (s *Service) markStopped(inst Instance, stopped bool) error {
	if inst.Stopped == stopped {
		return nil
	}
	inst.Stopped = stopped
	found, err := s.reg.Update(inst)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("实例不存在: %s", inst.ID)
	}
	s.logf("[INSTANCES] %s 人工停止标记 → %v", inst.ID, stopped)
	return nil
}

func (s *Service) stopLocked(id string) (StopResult, error) {
	inst, ok := s.reg.Get(id)
	if !ok {
		return StopResult{}, fmt.Errorf("实例不存在: %s", id)
	}
	if inst.IsSelf(s.opts.WebPort) {
		return StopResult{}, ErrSelfOperation
	}
	res := StopResult{Ctrl: []int{}, Robot: []int{}}
	// 先写人工停止标记（看门狗每轮重读注册表 → 跳过自动拉起），再杀进程。
	if err := s.markStopped(inst, true); err != nil {
		res.Warnings = append(res.Warnings, "人工停止标记写入失败（看门狗可能自动拉起）: "+err.Error())
	}
	k := s.stopInstance(inst)
	res.Ctrl = k.Ctrl
	res.Robot = k.Robot
	return res, nil
}

func (s *Service) stopInstance(inst Instance) StopResult {
	res := StopResult{Ctrl: []int{}, Robot: []int{}}
	for _, pid := range s.FindCtrlPIDs(inst.CtrlMatch) {
		if err := s.killPID(pid); err != nil {
			s.logf("[INSTANCES] 杀中控失败 pid=%d: %v", pid, err)
			continue
		}
		res.Ctrl = append(res.Ctrl, pid)
	}
	for _, pid := range s.FindRobotPIDs(inst.DeployDir) {
		if err := s.killPID(pid); err != nil {
			s.logf("[INSTANCES] 杀机器人失败 pid=%d: %v", pid, err)
			continue
		}
		res.Robot = append(res.Robot, pid)
	}
	s.logf("[INSTANCES] stop %s：中控 %v / 机器人 %v", inst.ID, res.Ctrl, res.Robot)
	return res
}

// ---------------------------------------------------------------- restart

// RestartResult 重启结果（robot 模式携带代理响应）。
type RestartResult struct {
	Mode    string
	Msg     string
	CtrlPID int
	// robot 模式：转发目标实例 /api/robot/restart 的响应
	ProxyStatus int
	ProxyBody   []byte
}

// Restart 重启实例：
//   - controller（默认）：杀本实例中控 → 跑 start_script（机器人不动，脚本是否带 --auto-robot 决定）；
//   - cold：先 stop（中控+机器人）→ 跑 start_script；
//   - robot：HTTP 代理目标实例 POST /api/robot/restart（转发响应；不外部杀进程）。
//
// 自身实例的 controller / cold 拒绝（ErrSelfOperation）。
func (s *Service) Restart(id, mode string) (RestartResult, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "controller"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.reg.Get(id)
	if !ok {
		return RestartResult{}, fmt.Errorf("实例不存在: %s", id)
	}
	switch mode {
	case "robot":
		status, body, err := s.proxyRobotRestart(inst)
		if err != nil {
			return RestartResult{}, err
		}
		s.logf("[INSTANCES] restart %s mode=robot（代理 :%d，HTTP %d）", id, inst.WebPort, status)
		return RestartResult{Mode: mode, ProxyStatus: status, ProxyBody: body}, nil
	case "controller", "cold":
		if inst.IsSelf(s.opts.WebPort) {
			return RestartResult{}, ErrSelfOperation
		}
		// 重启=意图恢复运行：清人工停止标记（与「启动」同口径）。
		if err := s.markStopped(inst, false); err != nil {
			s.logf("[INSTANCES] restart %s：清除人工停止标记失败: %v", id, err)
		}
		script := inst.StartScript
		if mode == "cold" {
			s.stopInstance(inst)
		} else {
			// controller：只杀中控进程（不动机器人）。
			// 2026-10-14 复核 P1-1：优先用 warm_script（不杀/不重拉机器人），缺省回退 start_script。
			for _, pid := range s.FindCtrlPIDs(inst.CtrlMatch) {
				if err := s.killPID(pid); err != nil {
					s.logf("[INSTANCES] 杀中控失败 pid=%d: %v", pid, err)
				}
			}
			if ws := strings.TrimSpace(inst.WarmScript); ws != "" {
				script = ws
			}
		}
		out, err := s.startLockedWith(inst, script)
		if err != nil {
			return RestartResult{}, err
		}
		return RestartResult{Mode: mode, Msg: out.Msg, CtrlPID: out.CtrlPID}, nil
	default:
		return RestartResult{}, fmt.Errorf("mode 必须是 controller/cold/robot，实际 %q", mode)
	}
}

// proxyRobotRestart 转发目标实例的 POST /api/robot/restart（30s 超时；机器人重启较慢）。
func (s *Service) proxyRobotRestart(inst Instance) (int, []byte, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/robot/restart", inst.WebPort)
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("转发 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// ---------------------------------------------------------------- delete

// DeleteResult 删除（归档）结果。
type DeleteResult struct {
	Archived map[string]string `json:"archived"` // 原目录 → 归档后路径
	Warnings []string          `json:"warnings"`
	Killed   StopResult        `json:"killed"`
}

// Delete 删除实例：先 stop → 目录 rename 归档（<dir>_archived_<YYYYMMDD>）→ 注册表移除。
//   - confirm 必须与 id 完全一致（防误删）；
//   - 自身实例拒绝；
//   - rename 失败记 warning 不阻断（宁可在注册表移除后留档，也不先删）。
func (s *Service) Delete(id, confirm string) (DeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.reg.Get(id)
	if !ok {
		return DeleteResult{}, fmt.Errorf("实例不存在: %s", id)
	}
	if strings.TrimSpace(confirm) != id {
		return DeleteResult{}, fmt.Errorf("confirm 与实例 id 不一致（需输入 %s）", id)
	}
	if inst.IsSelf(s.opts.WebPort) {
		return DeleteResult{}, ErrSelfOperation
	}
	// 归档=先停：先写人工停止标记（防"停→归档→移除"窗口内被看门狗拉起半归档实例）。
	markErr := s.markStopped(inst, true)
	killed := s.stopInstance(inst)
	res := DeleteResult{Archived: map[string]string{}, Warnings: []string{}, Killed: killed}
	if markErr != nil {
		res.Warnings = append(res.Warnings, "人工停止标记写入失败: "+markErr.Error())
	}
	tag := s.now().Format("20060102")
	for _, dir := range []string{inst.DataDir, inst.DeployDir} {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			res.Warnings = append(res.Warnings, "目录不存在，跳过归档: "+dir)
			continue
		}
		dst := dir + "_archived_" + tag
		if err := os.Rename(dir, dst); err != nil {
			res.Warnings = append(res.Warnings, "归档失败（不阻断）: "+dir+" → "+err.Error())
			continue
		}
		res.Archived[dir] = dst
	}
	if _, _, err := s.reg.Remove(id); err != nil {
		return res, fmt.Errorf("注册表移除失败: %w", err)
	}
	s.logf("[INSTANCES] delete %s：归档 %v 警告 %v", id, res.Archived, res.Warnings)
	return res, nil
}
