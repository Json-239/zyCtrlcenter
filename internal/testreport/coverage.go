// 覆盖现状.md 的"用例数"列：解析 / 校验 / 回写（纯函数）。
//
// 背景：这份表是人工维护的，数字迟早会漂（本仓实测：逐行相加 210、合计写 217）。
// 所以数字的**唯一来源**是 `go test -json` 的实测结果：
//   - `tools/test_report` 不带 -write 时校验，对不上就非零退出（把漂移变成可见失败）；
//   - 带 -write 时只重写「用例数」列与合计行，说明/链接等人工内容原样保留；
//   - 元测试（test/testreport）校验"合计 == 各行相加"，防住手改漏改。
package testreport

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var moduleCellRe = regexp.MustCompile(`^test/[A-Za-z0-9_-]+$`)

// CoverageRow 表格里的一行。
type CoverageRow struct {
	Line    int    `json:"line"`   // 原文行下标（从 0 开始）
	Module  string `json:"module"` // 例：test/api；合计行是 ""
	Count   int    `json:"count"`
	Bold    bool   `json:"bold"`    // 数字是否加粗（**217**）
	IsTotal bool   `json:"is_total"`
}

// Coverage 覆盖现状.md §1 总览表的解析结果。
type Coverage struct {
	Rows     []CoverageRow
	TotalIdx int // Rows 里合计行的下标；-1 = 没有合计行
}

// ParseCoverageTable 解析 markdown 中"首列是 `test/xxx` 或 **合计**"的表格行。
//
// 容错：只认能解析出数字的行；含 `test/xxx` 但不是表格的行（如正文里的反引号）忽略。
func ParseCoverageTable(md string) (*Coverage, error) {
	cov := &Coverage{TotalIdx: -1}
	for i, line := range strings.Split(md, "\n") {
		cells, ok := splitTableRow(line)
		if !ok {
			continue
		}
		name := normalizeCell(cells[0])
		count, bold, ok := parseCountCell(cells[1])
		if !ok {
			continue
		}
		switch {
		case moduleCellRe.MatchString(name):
			cov.Rows = append(cov.Rows, CoverageRow{Line: i, Module: name, Count: count, Bold: bold})
		case strings.Contains(name, "合计"):
			cov.Rows = append(cov.Rows, CoverageRow{Line: i, Count: count, Bold: bold, IsTotal: true})
			cov.TotalIdx = len(cov.Rows) - 1
		}
	}
	if len(cov.Rows) == 0 {
		return nil, fmt.Errorf("没解析到总览表（首列应是 `test/<模块>` 或 `合计`）")
	}
	return cov, nil
}

// SumRows 各行相加（不含合计行）——元测试用它校验"合计 == 各行相加"。
func (c *Coverage) SumRows() int {
	sum := 0
	for _, r := range c.Rows {
		if !r.IsTotal {
			sum += r.Count
		}
	}
	return sum
}

// Total 合计行的数字（没有合计行返回 -1）。
func (c *Coverage) Total() int {
	if c.TotalIdx < 0 {
		return -1
	}
	return c.Rows[c.TotalIdx].Count
}

// CoverageDiff 一处不一致。
type CoverageDiff struct {
	Module string `json:"module"`
	Doc    int    `json:"doc"`
	Actual int    `json:"actual"`
	Kind   string `json:"kind"` // count（数字不同）/ missing（文档里有、实测没有）/ unlisted（实测有、文档没列）
}

func (d CoverageDiff) String() string {
	switch d.Kind {
	case "missing":
		return fmt.Sprintf("%s：文档写 %d，实测没有这个测试包", d.Module, d.Doc)
	case "unlisted":
		return fmt.Sprintf("%s：实测 %d 个用例，但文档没列这一行", d.Module, d.Actual)
	default:
		return fmt.Sprintf("%s：文档 %d → 实测 %d", d.Module, d.Doc, d.Actual)
	}
}

// ValidateCoverage 对比文档与实测（含合计行）：返回按模块排序的差异清单。
func ValidateCoverage(cov *Coverage, actual map[string]int) []CoverageDiff {
	diffs := []CoverageDiff{}
	seen := map[string]bool{}
	sum := 0
	for _, r := range cov.Rows {
		if r.IsTotal {
			continue
		}
		seen[r.Module] = true
		n, ok := actual[r.Module]
		if !ok {
			diffs = append(diffs, CoverageDiff{Module: r.Module, Doc: r.Count, Kind: "missing"})
			sum += r.Count
			continue
		}
		sum += n
		if n != r.Count {
			diffs = append(diffs, CoverageDiff{Module: r.Module, Doc: r.Count, Actual: n})
		}
	}
	for mod, n := range actual {
		if seen[mod] || n == 0 {
			continue // 没用例的包（如 test/testsupport 只有辅助函数）不进表
		}
		diffs = append(diffs, CoverageDiff{Module: mod, Actual: n, Kind: "unlisted"})
	}
	if t := cov.Total(); t >= 0 && t != sum {
		diffs = append(diffs, CoverageDiff{Module: "合计", Doc: t, Actual: sum})
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Module < diffs[j].Module })
	return diffs
}

// RewriteCoverage 只改「用例数」列：模块行用实测数字，合计行用各行实测之和。
// 说明列、链接列、加粗样式原样保留；返回新文本与改动清单（无改动时新文本 == 原文）。
func RewriteCoverage(md string, cov *Coverage, actual map[string]int) (string, []CoverageDiff) {
	lines := strings.Split(md, "\n")
	diffs := []CoverageDiff{}

	total := 0
	for _, r := range cov.Rows {
		if r.IsTotal {
			continue
		}
		n, ok := actual[r.Module]
		if !ok {
			total += r.Count // 实测没有这个包：保留原数字（ValidateCoverage 会报 missing）
			continue
		}
		total += n
		if n == r.Count {
			continue
		}
		lines[r.Line] = replaceCountCell(lines[r.Line], n, r.Bold)
		diffs = append(diffs, CoverageDiff{Module: r.Module, Doc: r.Count, Actual: n})
	}
	if cov.TotalIdx >= 0 {
		r := cov.Rows[cov.TotalIdx]
		if r.Count != total {
			lines[r.Line] = replaceCountCell(lines[r.Line], total, r.Bold)
			diffs = append(diffs, CoverageDiff{Module: "合计", Doc: r.Count, Actual: total})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Module < diffs[j].Module })
	return strings.Join(lines, "\n"), diffs
}

// splitTableRow markdown 表格行 → 单元格（首尾空段去掉）。
func splitTableRow(line string) ([]string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") || !strings.HasSuffix(t, "|") || len(t) < 4 {
		return nil, false
	}
	parts := strings.Split(t, "|")
	if len(parts) < 4 {
		return nil, false
	}
	cells := make([]string, 0, len(parts)-2)
	for _, p := range parts[1 : len(parts)-1] {
		cells = append(cells, p)
	}
	if len(cells) < 2 {
		return nil, false
	}
	return cells, true
}

// normalizeCell 去掉反引号/加粗/空白：`test/api` / **合计** → test/api / 合计。
func normalizeCell(cell string) string {
	s := strings.TrimSpace(cell)
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "*", "")
	return strings.TrimSpace(s)
}

// parseCountCell 数字单元格 → (数字, 是否加粗)。
func parseCountCell(cell string) (int, bool, bool) {
	raw := strings.TrimSpace(cell)
	bold := strings.HasPrefix(raw, "**") && strings.HasSuffix(raw, "**")
	s := strings.ReplaceAll(raw, "*", "")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimSpace(s)
	if s == "" || s == "—" || s == "-" {
		return 0, bold, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, bold, false
		}
		n = n*10 + int(r-'0')
	}
	return n, bold, true
}

// replaceCountCell 只替换表格行的第 2 个单元格（用例数），其余原样。
func replaceCountCell(line string, n int, bold bool) string {
	t := strings.TrimSpace(line)
	parts := strings.Split(t, "|")
	if len(parts) < 4 {
		return line
	}
	val := fmt.Sprintf(" %d ", n)
	if bold {
		val = fmt.Sprintf(" **%d** ", n)
	}
	parts[2] = val
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	return indent + strings.Join(parts, "|")
}
