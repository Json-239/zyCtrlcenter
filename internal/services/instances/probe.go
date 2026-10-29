// 进程定位与状态探测（实例管理页 V1）。
//
// 定位方式（**唯一允许**）：
//   - 中控 PID：镜像名 zyctrlcenter.exe + 命令行包含 ctrl_match（如 `--data-dir "…\data-2300"`）；
//     绝不按镜像名盲杀——同机所有实例的中控都叫 zyctrlcenter.exe；
//   - 机器人 PID：镜像名 robot_single_robot.exe + ExecutablePath 在 deploy_dir 下
//     （路径+反斜杠边界，`single_robot_zy\` 不会命中 `single_robot_zy_2300\`，参考 process/orphans.go）。
//
// 红线：拿不到输出/解析失败一律视为空（宁可不动作）。
package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	ctrlImageName  = "zyctrlcenter.exe"       // 中控镜像名（同机多实例共用）
	robotImageName = "robot_single_robot.exe" // 机器人镜像名（同理）
)

// ---------------------------------------------------------------- PowerShell 脚本构建

// psQuote 把字符串包成 PowerShell 单引号字面量（内部单引号翻倍转义）。
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// containsExpr 生成 PowerShell 布尔表达式：<var> -like '*<literal转义>*'（包含匹配）。
//
// 用 [WildcardPattern]::Escape 对字面量整体转义：路径/命令行里的 [ ] * ? ` 等
// 通配字符全部按字面处理，避免匹配语义被带偏（单引号字符串里反引号无法转义，
// 因此不能用拼接式手工转义）。
func containsExpr(varName, literal string) string {
	return fmt.Sprintf("(%s -like ('*' + [System.Management.Automation.WildcardPattern]::Escape(%s) + '*'))",
		varName, psQuote(literal))
}

// prefixExpr 生成前缀匹配表达式：<var> -like '<literal转义>*'。
func prefixExpr(varName, literal string) string {
	return fmt.Sprintf("(%s -like ([System.Management.Automation.WildcardPattern]::Escape(%s) + '*'))",
		varName, psQuote(literal))
}

// listByCommandLineScript 列"命令行包含 literal"的进程 PID。
func listByCommandLineScript(imageName, literal string) string {
	return fmt.Sprintf("Get-CimInstance Win32_Process -Filter %s | Where-Object { %s } | Select-Object -ExpandProperty ProcessId",
		psQuote("Name='"+imageName+"'"), containsExpr("$_.CommandLine", literal))
}

// listByExePathScript 列"可执行文件在 dir 下"的进程 PID（边界=路径+反斜杠前缀）。
func listByExePathScript(imageName, dir string) string {
	dir = strings.TrimRight(dir, `\/`)
	return fmt.Sprintf("Get-CimInstance Win32_Process -Filter %s | Where-Object { %s } | Select-Object -ExpandProperty ProcessId",
		psQuote("Name='"+imageName+"'"), prefixExpr("$_.ExecutablePath", dir+`\`))
}

// parsePIDs 解析 PowerShell 输出的 PID 列表（纯函数，便于单测）。
func parsePIDs(out string) []int {
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

// runPowerShell 执行脚本取输出（8s 超时；失败返回 error）。
func runPowerShell(script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
}

// runPS 走注入的执行器（测试）或真实 PowerShell；非 Windows 平台不动作。
func (s *Service) runPS(script string) (string, bool) {
	if s.RunPowerShell != nil {
		out, err := s.RunPowerShell(script)
		if err != nil {
			return "", false
		}
		return string(out), true
	}
	if runtime.GOOS != "windows" {
		return "", false
	}
	out, err := runPowerShell(script)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// ---------------------------------------------------------------- 进程定位

// FindCtrlPIDs 按 ctrl_match 定位中控 PID（空/失败 → nil）。
func (s *Service) FindCtrlPIDs(match string) []int {
	match = strings.TrimSpace(match)
	if match == "" {
		return nil
	}
	out, ok := s.runPS(listByCommandLineScript(ctrlImageName, match))
	if !ok {
		return nil
	}
	return parsePIDs(out)
}

// FindRobotPIDs 按 deploy_dir 边界定位机器人 PID（空/失败 → nil）。
func (s *Service) FindRobotPIDs(deployDir string) []int {
	deployDir = strings.TrimSpace(deployDir)
	if deployDir == "" {
		return nil
	}
	out, ok := s.runPS(listByExePathScript(robotImageName, deployDir))
	if !ok {
		return nil
	}
	return parsePIDs(out)
}

// killPID 杀单个进程（taskkill /PID /F；只按 PID，**绝不 /IM 全杀**）。
func (s *Service) killPID(pid int) error {
	if s.KillPID != nil {
		return s.KillPID(pid)
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("非 Windows 平台不支持杀进程")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/F").Run()
}

// ---------------------------------------------------------------- 状态探测

// Status 一个实例的实时状态（契约字段名固定）。
type Status struct {
	CtrlAlive  bool  `json:"ctrl_alive"`
	CtrlPID    int   `json:"ctrl_pid"`
	HTTPOK     bool  `json:"http_ok"`
	Online     int   `json:"online"`
	RobotAlive bool  `json:"robot_alive"`
	RobotPID   int   `json:"robot_pid"`
	CheckedAt  int64 `json:"checked_at"`
}

// Status 实时探测（进程定位 + HTTP 探活）。
func (s *Service) Status(inst Instance) Status {
	st := Status{CheckedAt: nowUnix(), Online: -1}
	if pids := s.FindCtrlPIDs(inst.CtrlMatch); len(pids) > 0 {
		st.CtrlAlive, st.CtrlPID = true, pids[0]
	}
	if pids := s.FindRobotPIDs(inst.DeployDir); len(pids) > 0 {
		st.RobotAlive, st.RobotPID = true, pids[0]
	}
	if inst.WebPort > 0 {
		st.HTTPOK, st.Online = s.probeHTTP(inst.WebPort)
	}
	return st
}

// probeHTTP 探活实例 HTTP：
//  1. GET /api/ping（1.5s 超时）——本版实例的轻量探活端点；
//  2. GET /api/status（1.5s）——兼容旧版实例（无 ping 端点）并取 counts.online；
//
// http_ok = 任一成功；online 只由 /api/status 提供（未取到 = -1）。
func (s *Service) probeHTTP(webPort int) (bool, int) {
	client := s.httpClient()
	pingOK := httpOK(client, fmt.Sprintf("http://127.0.0.1:%d/api/ping", webPort))
	online := -1
	statusOK := false
	if doc, ok := httpGetJSON(client, fmt.Sprintf("http://127.0.0.1:%d/api/status", webPort)); ok {
		statusOK = true
		if counts, ok := doc["counts"].(map[string]any); ok {
			if v, ok := counts["online"].(float64); ok {
				online = int(v)
			}
		}
	}
	return pingOK || statusOK, online
}

func (s *Service) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 1500 * time.Millisecond}
}

// httpOK 请求 URL 且 200 + 合法 JSON。
func httpOK(client *http.Client, url string) bool {
	_, ok := httpGetJSON(client, url)
	return ok
}

// httpGetJSON GET 并解析 JSON（非 200 / 解析失败 → false）。
func httpGetJSON(client *http.Client, url string) (map[string]any, bool) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, false
	}
	return doc, true
}

// ---------------------------------------------------------------- 端口预检

// portListenable 端口可否试绑（成功即关；失败返回占用/不可用原因）。
//
// 两次试绑：127.0.0.1（中控实际监听地址——Windows 允许 wildcard 与 specific 并存，
// 只试 ":port" 会漏检"已有实例占着 127.0.0.1:port"）+ ":port"（通配占用）。
func portListenable(port int) error {
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf(":%d", port)} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		if err := ln.Close(); err != nil {
			return err
		}
	}
	return nil
}
