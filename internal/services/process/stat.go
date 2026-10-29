package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrNotRunning 部署目录下未发现机器人进程。
var ErrNotRunning = errors.New("部署目录下未发现机器人进程")

// Stat 机器人进程运行态（2026-10-27 内存护栏用）。
//
// 口径：内存同时给工作集（物理驻留）与提交量（私有字节）。事故是主机**提交内存**
// 被耗尽（WER 2004 报的就是虚拟内存），所以护栏判定取两者较大值（MemMB）。
type Stat struct {
	PID          int
	WorkingSetMB float64       // 工作集（物理驻留）
	CommitMB     float64       // 提交量（PrivatePageCount）
	Uptime       time.Duration // 进程已运行时长
	Peers        int           // 同部署目录下的进程数（>1 说明有孤儿）
}

// MemMB 护栏判定口径：取工作集/提交量较大值。
func (s Stat) MemMB() float64 {
	if s.CommitMB > s.WorkingSetMB {
		return s.CommitMB
	}
	return s.WorkingSetMB
}

// statRunner 执行采样脚本并返回 stdout（测试可注入 fake）。
type statRunner func(ctx context.Context, script string) (string, error)

// SetStatRunner 替换采样执行器（仅测试用；nil 恢复真实 PowerShell）。
func (m *Manager) SetStatRunner(fn func(ctx context.Context, script string) (string, error)) {
	m.mu.Lock()
	m.statRun = fn
	m.mu.Unlock()
}

// statScript PowerShell 采样脚本：只取「可执行文件落在部署目录下」的进程
// （与 orphans.go 同一安全红线：绝不按镜像名跨目录匹配，同机还有 py/测试栈同名进程）。
//
// 输出每个进程一行 csv：pid,WorkingSetSize,PrivatePageCount,uptimeSec。
// uptime 用时间差整数秒，避免解析 DMTF 时间字符串（时区坑）。
func statScript(imageName, dir string) string {
	img := strings.ReplaceAll(imageName, "'", "''")
	d := strings.ReplaceAll(strings.TrimRight(dir, `\/`), "'", "''")
	return fmt.Sprintf(
		`Get-CimInstance Win32_Process -Filter "Name='%s'" | Where-Object { $_.ExecutablePath -like '%s\*' } | `+
			`ForEach-Object { '{0},{1},{2},{3}' -f $_.ProcessId,$_.WorkingSetSize,$_.PrivatePageCount,`+
			`$(if ($_.CreationDate) { [int]((Get-Date)-$_.CreationDate).TotalSeconds } else { 0 }) }`,
		img, d)
}

// defaultStatRunner 真实执行 PowerShell。
func defaultStatRunner(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.Output()
	return string(out), err
}

// Stat 采样部署目录下的机器人进程（只读，不杀不拉）。
//
// Windows 用 PowerShell 按路径过滤（兼容"中控重启后 m.cmd=nil"的孤儿场景）；
// 其它平台只认中控自己拉起的进程（没有跨目录探测能力）。
func (m *Manager) Stat() (Stat, error) {
	if runtime.GOOS != "windows" {
		m.mu.Lock()
		cmd, exited := m.cmd, m.exited
		m.mu.Unlock()
		if cmd == nil || cmd.Process == nil || exited {
			return Stat{}, ErrNotRunning
		}
		return Stat{PID: cmd.Process.Pid, Peers: 1}, nil
	}

	m.mu.Lock()
	dir := strings.TrimSpace(m.deployDir)
	image := m.imageName
	run := m.statRun
	m.mu.Unlock()

	if dir == "" {
		return Stat{}, fmt.Errorf("部署目录未配置")
	}
	if run == nil {
		run = defaultStatRunner
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	out, err := run(ctx, statScript(image, dir))
	if err != nil {
		return Stat{}, fmt.Errorf("采样机器人进程失败: %w", err)
	}
	return ParseStatOut(out)
}

// ParseStatOut 解析采样输出（纯函数，便于单测）。
//
// 容忍：CRLF、空行、PowerShell 错误文本；多进程时取 CommitMB 最大者为主进程，
// Peers 记录进程总数（>1 提示有孤儿，由调用方告警）。
func ParseStatOut(out string) (Stat, error) {
	var best Stat
	n := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\r"))
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 4 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		ws, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		commit, err3 := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		up, err4 := strconv.Atoi(strings.TrimSpace(parts[3]))
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || pid <= 0 {
			continue
		}
		n++
		s := Stat{
			PID:          pid,
			WorkingSetMB: float64(ws) / 1048576,
			CommitMB:     float64(commit) / 1048576,
			Uptime:       time.Duration(up) * time.Second,
		}
		if n == 1 || s.CommitMB > best.CommitMB {
			best = s
		}
	}
	if n == 0 {
		return Stat{}, ErrNotRunning
	}
	best.Peers = n
	return best, nil
}
