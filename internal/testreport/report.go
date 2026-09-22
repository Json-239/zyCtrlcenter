// 测试报告的数据结构与 Markdown 渲染（纯函数）。
//
// 两个产物：
//   - docs/04-测试/报告-YYYYMMDD.md（人看的：汇总 / 关键保障 / 失败项 / 复现方式）；
//   - data/test_report.json（机器看的：面板「模块地图」页读它显示"最近一次测试结果"）。
package testreport

import (
	"fmt"
	"strings"
)

// ChainFile 链数据文件的指纹（报告要能回答"跑的是哪份链数据"）。
type ChainFile struct {
	File string `json:"file"`
	Size int64  `json:"size"`
	SHA1 string `json:"sha1"`
}

// Env 跑测试的环境。
type Env struct {
	OS         string      `json:"os"`
	Go         string      `json:"go"`
	ChainDir   string      `json:"chain_dir"`
	Chains     []ChainFile `json:"chains"`
	GameConfig string      `json:"game_config"`
}

// Totals 合计。
type Totals struct {
	Cases   int     `json:"cases"`
	Passed  int     `json:"passed"`
	Failed  int     `json:"failed"`
	Skipped int     `json:"skipped"`
	Elapsed float64 `json:"elapsed"`
}

// Report 一次测试报告（JSON 进 data/test_report.json，Markdown 进 docs/04-测试/）。
type Report struct {
	Date        string    `json:"date"`         // YYYY-MM-DD
	GeneratedAt string    `json:"generated_at"` // 本地时间
	OK          bool      `json:"ok"`           // vet + 全量测试（+竞态，若跑了）全绿
	Env         Env       `json:"env"`
	Totals      Totals    `json:"totals"`
	Packages    []Package `json:"packages"`
	Failures    []Failure `json:"failures"`
	Gate        []string  `json:"gate"`  // 链数据客观闸门的 t.Logf 结论
	Vet         string    `json:"vet"`   // ok / failed / skipped
	Race        string    `json:"race"`  // ok / failed / skipped
	VetOut      string    `json:"vet_out,omitempty"`
	RaceOut     string    `json:"race_out,omitempty"`
	Cmd         string    `json:"cmd"`    // 复现命令
	ReportPath  string    `json:"report"` // 生成的 Markdown 路径
	Coverage    []CoverageDiff `json:"coverage,omitempty"` // 覆盖现状.md 的差异（无差异为空）
}

// GateOK 闸门是否给出过结论（没给 = 本机没跑客观校验：缺游戏服配置/链目录）。
func (r Report) GateOK() bool { return len(r.Gate) > 0 }

// RenderMarkdown 按固定四节渲染（§1 汇总 / §2 关键保障 / §3 失败项 / §4 复现方式）。
func (r Report) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 测试报告 %s\n\n", r.Date)
	fmt.Fprintf(&b, "环境：%s / Go %s / 链目录 %s / 游戏配置 %s\n\n",
		r.Env.OS, r.Env.Go, orDash(r.Env.ChainDir), orDash(r.Env.GameConfig))
	if len(r.Env.Chains) > 0 {
		parts := make([]string, 0, len(r.Env.Chains))
		for _, c := range r.Env.Chains {
			parts = append(parts, fmt.Sprintf("%s sha1:%s（%d 字节）", c.File, c.SHA1, c.Size))
		}
		fmt.Fprintf(&b, "链数据：%s\n\n", strings.Join(parts, " · "))
	}
	fmt.Fprintf(&b, "结论：%s（vet=%s，竞态=%s，用例 %d：通过 %d / 失败 %d / 跳过 %d，耗时 %.1fs）\n\n",
		map[bool]string{true: "**全绿**", false: "**有失败**"}[r.OK], r.Vet, r.Race,
		r.Totals.Cases, r.Totals.Passed, r.Totals.Failed, r.Totals.Skipped, r.Totals.Elapsed)

	b.WriteString("## 1. 汇总\n\n")
	b.WriteString("| 模块 | 用例 | 通过 | 失败 | 跳过 | 耗时(s) |\n|---|---|---|---|---|---|\n")
	for _, p := range r.Packages {
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %.2f |\n",
			p.Short, p.Cases, p.Passed, p.Failed, p.Skipped, p.Elapsed)
	}
	fmt.Fprintf(&b, "| **合计** | **%d** | **%d** | **%d** | **%d** | **%.2f** |\n\n",
		r.Totals.Cases, r.Totals.Passed, r.Totals.Failed, r.Totals.Skipped, r.Totals.Elapsed)

	b.WriteString("## 2. 关键保障（断言级）\n\n")
	b.WriteString("- **机器人侧实收命令**：`test/api`（自动分配 / 抓鬼带导航载荷 / 补发合并）逐条断言下发内容；`test/ctrl` 断言通道行为\n")
	b.WriteString("- **我们这侧**：`test/chainlib`·`test/chainplan`·`test/chaindata` 覆盖链数据搬运、模块组装与客观可达性\n")
	if r.GateOK() {
		b.WriteString("- **链数据客观闸门**（只用游戏服本机 blockfile，不引用任何一方结论）：\n\n")
		for _, g := range r.Gate {
			fmt.Fprintf(&b, "  - %s\n", g)
		}
	} else {
		b.WriteString("- **链数据客观闸门**：本次未跑（缺游戏服配置目录或链数据目录）；设 `ZY_GAME_CONFIG=<游戏服 config>` 后重跑\n")
	}
	b.WriteString("\n")

	b.WriteString("## 3. 失败项\n\n")
	if len(r.Failures) == 0 {
		b.WriteString("无\n\n")
	} else {
		b.WriteString("| 用例 | 首行断言信息 |\n|---|---|\n")
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "| `%s::%s` | %s |\n", f.Package, f.Test, escapeCell(f.FirstLine))
		}
		b.WriteString("\n")
	}

	b.WriteString("## 4. 复现方式\n\n")
	b.WriteString("```bat\nrun_tests.bat\n```\n\n")
	fmt.Fprintf(&b, "报告工具：`%s`\n\n", orDash(r.Cmd))
	if len(r.Coverage) > 0 {
		b.WriteString("覆盖现状.md 与实测不一致（用 `go run ./tools/test_report -write` 回写）：\n\n")
		for _, d := range r.Coverage {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if strings.TrimSpace(s) == "" {
		return "（无输出）"
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
