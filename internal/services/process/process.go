// Package process 机器人进程管理（对标 Python 版 services/process.py）。
//
// 两种来源的进程：
//   - 中控自己拉起的（持有 exec.Cmd，可精确感知退出）；
//   - 外部/残留进程（Windows 下用 tasklist 按镜像名探测）。
//
// 默认「不动别人的进程」：kill 只影响自己拉起的进程，清理同名残留需显式传 killOrphans。
package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Manager 机器人进程管理器（并发安全）。
type Manager struct {
	exePath   string
	deployDir string
	imageName string
	logf      func(string, ...any)

	// extraEnv 额外环境变量（每次拉起时求值）。壳层用它把"当前区"的游戏服地址
	// 注入给机器人进程：机器人 config.py 读 ROBOT_ZONE_IP/ROBOT_ZONE_PORT
	// （缺省回落到 47.96.8.240:2300）。这样面板切区 + 重启机器人即可整体切服。
	extraEnv func() []string

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited bool
	waitCh chan struct{}

	// RunningAny 缓存（tasklist 有开销，面板 3 秒轮询不能每次都查系统进程）
	lastAnyCheck time.Time
	lastAnyValue bool
}

// SetExtraEnv 设置"拉起机器人时追加的环境变量"（nil 表示不追加）。
func (m *Manager) SetExtraEnv(fn func() []string) { m.extraEnv = fn }

// exePathOf 部署目录 → 机器人可执行文件路径（与 config.RobotExe 约定一致）。
func exePathOf(deployDir string) string {
	return filepath.Join(deployDir, "robot_single_robot.exe")
}

// New 创建管理器。imageName 用于 tasklist/taskkill（如 robot_single_robot.exe）。
func New(exePath, deployDir, imageName string, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if imageName == "" {
		imageName = filepath.Base(exePath)
	}
	return &Manager{exePath: exePath, deployDir: deployDir, imageName: imageName, logf: logf}
}

// ExePath 返回机器人可执行文件路径。
func (m *Manager) ExePath() string { return m.exePath }

// ExeExists 可执行文件是否存在。
func (m *Manager) ExeExists() bool {
	st, err := os.Stat(m.exePath)
	return err == nil && !st.IsDir()
}

// Running 中控自己拉起的进程是否存活。
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil && !m.exited
}

// RunningAny 系统内是否存在该镜像进程（Windows 用 tasklist；其它平台只认自己拉起的）。
// 结果缓存 5 秒（面板 3 秒轮询，避免频繁调用系统命令）。
func (m *Manager) RunningAny() bool {
	if m.Running() {
		return true
	}
	if runtime.GOOS != "windows" {
		return false
	}
	m.mu.Lock()
	if time.Since(m.lastAnyCheck) < 5*time.Second {
		v := m.lastAnyValue
		m.mu.Unlock()
		return v
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tasklist", "/FI", "IMAGENAME eq "+m.imageName, "/NH").Output()
	found := err == nil && strings.Contains(strings.ToLower(string(out)), strings.ToLower(m.imageName))

	m.mu.Lock()
	m.lastAnyCheck = time.Now()
	m.lastAnyValue = found
	m.mu.Unlock()
	return found
}

// EnsureRunning 未运行则拉起机器人进程；exe 缺失返回错误。
func (m *Manager) EnsureRunning() error {
	if m.Running() {
		return nil
	}
	if !m.ExeExists() {
		return fmt.Errorf("机器人程序不存在: %s", m.exePath)
	}
	// 拉起前先清"同目录的其它机器人进程"：中控重启后旧进程会成孤儿，否则会起出第二个，
	// 两个进程抢同一条控制通道（单连接：后连的接管）。只按部署目录匹配，不碰其它中控的机器人。
	m.killOrphansInDeployDir()
	cmd := exec.Command(m.exePath)
	cmd.Dir = m.deployDir
	cmd.SysProcAttr = hideWindowAttr()
	// 2026-09-22 注入当前区地址（ROBOT_ZONE_IP/PORT）：切区后重启机器人即整体切服。
	if m.extraEnv != nil {
		if extra := m.extraEnv(); len(extra) > 0 {
			cmd.Env = append(os.Environ(), extra...)
			m.logf("[PROC] 注入机器人环境: %v", extra)
		}
	}
	// 机器人自己把连接/登录统计写 console_<server>.log，这里 stdout 直接丢弃，
	// 避免与机器人自身日志重复。
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动机器人失败: %w", err)
	}
	waitCh := make(chan struct{})
	m.mu.Lock()
	m.cmd = cmd
	m.exited = false
	m.waitCh = waitCh
	m.mu.Unlock()
	m.logf("[PROC] 机器人已启动 pid=%d exe=%s", cmd.Process.Pid, m.exePath)

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.exited = true
		close(waitCh)
		m.mu.Unlock()
		m.logf("[PROC] 机器人进程退出: %v", err)
	}()
	return nil
}

// Stop 停止机器人：
//   - killOrphans=true：Windows 下 taskkill /IM 按镜像名清理（含残留）；
//   - killOrphans=false：只结束中控自己拉起的进程，不动外部进程。
//
// 返回是否有进程被结束。
func (m *Manager) Stop(killOrphans bool) bool {
	killed := false
	if killOrphans {
		// 只清理"可执行文件在我们部署目录下"的残留 —— **不按镜像名**：同机可能还有
		// py 中控(deploy/single_robot) / 测试中控(deploy_test) 的机器人，镜像名一模一样。
		if n := m.killOrphansInDeployDir(); n > 0 {
			killed = true
		}
	}
	m.mu.Lock()
	cmd := m.cmd
	m.cmd = nil
	m.exited = true
	waitCh := m.waitCh
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		killed = true
	}
	if waitCh != nil {
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
		}
	}
	return killed
}

// Restart 重启机器人（killOrphans 控制是否清理同名残留）。
func (m *Manager) Restart(killOrphans bool) error {
	m.Stop(killOrphans)
	time.Sleep(500 * time.Millisecond)
	return m.EnsureRunning()
}
