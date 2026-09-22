// 按「部署目录」清理同目录残留的机器人进程（Windows）。
//
// 为什么必须有：中控重启后，旧机器人进程会变成**孤儿**（新中控手里没有它的句柄），
// 而 EnsureRunning 只看"自己拉起的 cmd"→ 判定"没在跑"→ 再拉一个 →
// **同目录两个机器人抢同一条控制通道**（单连接语义：后连的接管旧的）→ 连接/状态抖动。
//
// 安全红线：**只按可执行文件路径过滤**（可执行文件落在我们部署目录下的才算"我们的残留"）。
// 绝不能按镜像名清 —— 同一台机器上可能同时跑着 py 中控的机器人（deploy/single_robot）、
// 测试中控的机器人（deploy_test），镜像名完全相同（robot_single_robot.exe）。
package process

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// listByExePathScript PowerShell 脚本：列出镜像名匹配、且**可执行文件在 dir 下**的进程 PID。
//
// 用路径前缀过滤（-like 'dir\*'）；dir 来自配置，不含通配符，无需转义。
func listByExePathScript(imageName, dir string) string {
	return fmt.Sprintf(
		"Get-CimInstance Win32_Process -Filter \"Name='%s'\" | Where-Object { $_.ExecutablePath -like '%s\\*' } | Select-Object -ExpandProperty ProcessId",
		imageName, strings.TrimRight(dir, `\/`))
}

// orphanPIDs 列出"可执行文件在我们部署目录下"的机器人进程 PID（不含我们自己拉起的那个）。
func (m *Manager) orphanPIDs() []int {
	if runtime.GOOS != "windows" || strings.TrimSpace(m.deployDir) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		listByExePathScript(m.imageName, m.deployDir)).Output()
	if err != nil {
		return nil // 拿不到就跳过（宁可不清理，也别乱杀）
	}
	own := 0
	if m.cmd != nil && m.cmd.Process != nil {
		own = m.cmd.Process.Pid
	}
	pids := ParsePIDs(string(out))
	kept := make([]int, 0, len(pids))
	for _, p := range pids {
		if p != own {
			kept = append(kept, p)
		}
	}
	return kept
}

// killOrphansInDeployDir 清理同目录残留（返回清理个数）。只在"准备拉起新进程"前调用。
func (m *Manager) killOrphansInDeployDir() int {
	pids := m.orphanPIDs()
	for _, pid := range pids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/F").Run()
		cancel()
		if err == nil {
			m.logf("[PROC] 清理同目录残留机器人进程 pid=%d（只按部署目录匹配，不碰其它中控的机器人）", pid)
		}
	}
	return len(pids)
}

// ParsePIDs 解析 PowerShell 输出的 PID 列表（纯函数，便于单测）。
func ParsePIDs(out string) []int {
	pids := []int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\r"))
		if line == "" {
			continue
		}
		if n, err := strconv.Atoi(line); err == nil && n > 0 {
			pids = append(pids, n)
		}
	}
	return pids
}
