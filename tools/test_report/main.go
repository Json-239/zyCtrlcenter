// test_report 一键跑全量测试并产出**可复现**的测试报告。
//
//	go run ./tools/test_report            # 跑 vet + 全量测试（+竞态，若本机有 gcc）→ 写报告
//	                                       # 顺便校验 覆盖现状.md 的「用例数」，不一致 → 非零退出
//	go run ./tools/test_report -write      # 同上，但把「用例数」列与合计回写进 覆盖现状.md
//	go run ./tools/test_report -race=off   # 显式不跑竞态检查
//
// 产物：
//   - docs/04-测试/报告-YYYYMMDD.md   人看的（汇总 / 关键保障 / 失败项 / 复现方式）
//   - data/test_report.json           机器看的（面板「模块地图」页读它显示最近一次结果）
//
// 数字口径：**唯一来源是 `go test -json` 的实测结果**，文档里的数字由 -write 回写、由默认校验
// 兜住（这是"测试框架数据可靠性"的机制：漂了就红，不会静默）。
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"zyctrlcenter/internal/testreport"
)

func main() {
	var (
		write      = flag.Bool("write", false, "把实测用例数回写进 docs/04-测试/覆盖现状.md（只改「用例数」列与合计）")
		race       = flag.String("race", "auto", "竞态检查：auto（有 gcc 才跑）/ on / off")
		reportsDir = flag.String("reports", filepath.Join("docs", "04-测试"), "报告输出目录")
		jsonPath   = flag.String("json", filepath.Join("data", "test_report.json"), "报告 JSON（面板读它）")
		coverageMD = flag.String("coverage", filepath.Join("docs", "04-测试", "覆盖现状.md"), "覆盖现状.md（校验/回写用例数）")
		chainDir   = flag.String("chain-dir", filepath.Join("data", "chains"), "链数据目录（报告里记指纹）")
	)
	flag.Parse()

	if err := run(*write, *race, *reportsDir, *jsonPath, *coverageMD, *chainDir); err != nil {
		fmt.Fprintf(os.Stderr, "\n[test_report] %v\n", err)
		os.Exit(1)
	}
}

func run(write bool, raceMode, reportsDir, jsonPath, coverageMD, chainDir string) error {
	goExe, err := goExecutable()
	if err != nil {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("请在项目根目录运行（找不到 go.mod）：%s", root)
	}

	// 1) go vet
	vetState, vetOut := runCmd(goExe, root, 10*time.Minute, "vet", "./...")
	fmt.Printf("[1/4] go vet ./... → %s\n", vetState)

	// 2) go test -json（统计的真正来源）
	jsonOut, testState, testErr := runCmdCapture(goExe, root, 20*time.Minute, "test", "./test/...", "-count=1", "-json")
	summary, err := testreport.Summarize(bytes.NewReader(jsonOut))
	if err != nil {
		return fmt.Errorf("解析 go test -json 输出失败: %w", err)
	}
	fmt.Printf("[2/4] go test ./test/... -count=1 → %s（%s）\n", testState, summary)
	_ = testErr // 测试失败通过 summary 体现；这里不提前退出，报告要写完整

	// 3) 竞态检查（可选）
	raceState, raceOut := "skipped", ""
	switch raceMode {
	case "off":
	case "on":
		raceState, raceOut = runCmd(goExe, root, 20*time.Minute, "test", "./test/...", "-race", "-count=1")
	default:
		if _, err := exec.LookPath("gcc"); err == nil {
			raceState, raceOut = runCmd(goExe, root, 20*time.Minute, "test", "./test/...", "-race", "-count=1")
		} else {
			raceOut = "未找到 gcc：-race 需要 CGO 工具链（装 mingw-w64 后自动启用）"
		}
	}
	fmt.Printf("[3/4] 竞态检查 → %s\n", raceState)

	// 4) 覆盖现状.md 的数字：默认校验，-write 时回写
	covDiffs, err := checkCoverage(coverageMD, summary, write)
	if err != nil {
		return err
	}
	if len(covDiffs) == 0 {
		fmt.Println("[4/4] 覆盖现状.md 用例数与实测一致")
	} else {
		fmt.Printf("[4/4] 覆盖现状.md 有 %d 处不一致：\n", len(covDiffs))
		for _, d := range covDiffs {
			fmt.Printf("      - %s\n", d)
		}
		if write {
			fmt.Println("      已回写（说明/链接等人工内容未动）")
		}
	}

	// 组装报告
	now := time.Now()
	report := testreport.Report{
		Date:        now.Format("2006-01-02"),
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Env:         buildEnv(goExe, root, chainDir),
		Totals: testreport.Totals{
			Cases: summary.Cases, Passed: summary.Passed, Failed: summary.Failed,
			Skipped: summary.Skipped, Elapsed: summary.Elapsed,
		},
		Packages: summary.Packages,
		Failures: summary.Failures,
		Gate:     summary.SortedGate(),
		Vet:      vetState,
		Race:     raceState,
		VetOut:   tail(vetOut, 800),
		RaceOut:  tail(raceOut, 800),
		Cmd:      "go run ./tools/test_report",
		Coverage: covDiffs,
	}
	report.OK = vetState == "ok" && summary.Failed == 0 && (raceState == "ok" || raceState == "skipped")

	mdPath := filepath.Join(reportsDir, "报告-"+now.Format("20060102")+".md")
	report.ReportPath = filepath.ToSlash(mdPath)
	if err := writeFile(mdPath, []byte(report.RenderMarkdown())); err != nil {
		return err
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(jsonPath, append(body, '\n')); err != nil {
		return err
	}
	fmt.Printf("\n报告已写出：%s\n           %s\n", mdPath, jsonPath)

	// 退出码：vet/测试/竞态/文档数字 任一不干净 → 非零（让漂移和失败都变成可见的失败）
	if !report.OK {
		return fmt.Errorf("测试未全绿（vet=%s，用例失败 %d，竞态=%s）", vetState, summary.Failed, raceState)
	}
	if len(covDiffs) > 0 {
		return fmt.Errorf("覆盖现状.md 与实测不一致（%d 处）：跑 go run ./tools/test_report -write 回写，"+
			"或先看看是不是漏登记了测试包", len(covDiffs))
	}
	return nil
}

// checkCoverage 校验（或回写）覆盖现状.md 的用例数；返回差异清单。
func checkCoverage(path string, sum *testreport.Summary, write bool) ([]testreport.CoverageDiff, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w（用 -coverage 指定路径）", path, err)
	}
	cov, err := testreport.ParseCoverageTable(string(raw))
	if err != nil {
		return nil, fmt.Errorf("解析 %s 的总览表失败: %w", path, err)
	}
	actual := sum.CaseCounts()
	if !write {
		return testreport.ValidateCoverage(cov, actual), nil
	}
	next, diffs := testreport.RewriteCoverage(string(raw), cov, actual)
	if len(diffs) > 0 {
		if err := writeFile(path, []byte(next)); err != nil {
			return nil, err
		}
	}
	// 回写后再校验一次：documented == actual 才算干净
	cov2, err := testreport.ParseCoverageTable(next)
	if err != nil {
		return nil, err
	}
	return testreport.ValidateCoverage(cov2, actual), nil
}

// buildEnv 采集环境信息（OS / Go 版本 / 链数据指纹 / 游戏配置路径）。
func buildEnv(goExe, root, chainDir string) testreport.Env {
	env := testreport.Env{
		OS:         osLabel(),
		Go:         goVersion(goExe, root),
		ChainDir:   filepath.ToSlash(chainDir),
		GameConfig: gameConfigDir(),
	}
	entries, err := os.ReadDir(chainDir)
	if err != nil {
		return env
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "_") {
			continue
		}
		path := filepath.Join(chainDir, name)
		if info, err := e.Info(); err == nil {
			env.Chains = append(env.Chains, testreport.ChainFile{
				File: name, Size: info.Size(), SHA1: fileSHA1(path),
			})
		}
	}
	sort.Slice(env.Chains, func(i, j int) bool { return env.Chains[i].File < env.Chains[j].File })
	return env
}

func gameConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("ZY_GAME_CONFIG")); v != "" {
		return filepath.ToSlash(v)
	}
	return `F:\ZyBin\xm\2d-xiyou-server\config` // 与 test/chaindata 的默认值一致
}

func osLabel() string {
	return runtime.GOOS + " " + runtime.GOARCH
}

// goExecutable 找 go（与 run_tests.bat 同口径：GOEXE 环境变量 → 常见安装路径 → PATH）。
func goExecutable() (string, error) {
	if v := strings.TrimSpace(os.Getenv("GOEXE")); v != "" {
		return v, nil
	}
	if p := `C:\Program Files\Go\bin\go.exe`; fileExists(p) {
		return p, nil
	}
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 go.exe：装 Go 或设 GOEXE=<go.exe 完整路径>")
}

func goVersion(goExe, dir string) string {
	out, _, _ := runCmdCapture(goExe, dir, 30*time.Second, "version")
	// "go version go1.24.3 windows/amd64" → "go1.24.3 windows/amd64"
	s := strings.TrimSpace(string(out))
	s = strings.TrimPrefix(s, "go version ")
	return s
}

// runCmd 跑一条命令，返回（ok/failed, 输出尾部）。
func runCmd(name, dir string, timeout time.Duration, args ...string) (string, string) {
	out, _, err := runCmdCapture(name, dir, timeout, args...)
	if err != nil {
		return "failed", tail(string(out), 4000)
	}
	return "ok", tail(string(out), 4000)
}

// runCmdCapture 跑一条命令，返回（stdout, 状态, error）。stderr 并入 stdout（报告要能看原因）。
func runCmdCapture(name, dir string, timeout time.Duration, args ...string) ([]byte, string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, "failed", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return buf.Bytes(), "failed", err
		}
		return buf.Bytes(), "ok", nil
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return buf.Bytes(), "failed", fmt.Errorf("超时（%s）", timeout)
	}
}

func fileSHA1(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha1.Sum(raw)
	return hex.EncodeToString(sum[:])
}

func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
