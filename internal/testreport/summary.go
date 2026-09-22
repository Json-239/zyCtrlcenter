// Package testreport 测试报告的**纯函数**部分：解析 `go test -json` 输出、汇总统计、
// 抽取链数据闸门结论、生成/校验文档里的用例数。
//
// 定位（为什么单独抽出来）：
//   - 手工维护的"用例数"一定会漂（本仓就发生过：表格逐行相加 210、合计写 217）；
//   - 所以数字**只有一个来源**：`go test -json` 的实际结果。工具（tools/test_report）负责跑
//     测试并调用本包做纯计算，测试（test/testreport）直接喂样例 JSONL 验证统计口径。
//
// 本包不做 IO、不跑命令：输入输出都是字符串/结构体，便于反复测。
package testreport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Event `go test -json` 的一行（只取用得到的字段）。
type Event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// Package 一个测试包（对应 test/<模块>）。
type Package struct {
	Name    string  `json:"name"`  // 全名，例：zyctrlcenter/test/api
	Short   string  `json:"short"` // 短名，例：test/api（与覆盖现状.md 的写法一致）
	Cases   int     `json:"cases"` // 顶层用例数（func Test*；子测试不计入）
	Passed  int     `json:"passed"`
	Failed  int     `json:"failed"`
	Skipped int     `json:"skipped"`
	Elapsed float64 `json:"elapsed"`
}

// Failure 一条失败用例（报告只列首行断言信息，细节去看测试输出）。
type Failure struct {
	Package   string `json:"package"`
	Test      string `json:"test"`
	FirstLine string `json:"first_line"`
}

// Summary 全量汇总。
type Summary struct {
	Packages []Package `json:"packages"`
	Cases    int       `json:"cases"`
	Passed   int       `json:"passed"`
	Failed   int       `json:"failed"`
	Skipped  int       `json:"skipped"`
	Elapsed  float64   `json:"elapsed"`
	Failures []Failure `json:"failures"`
	Gate     []string  `json:"gate"` // test/chaindata 的闸门结论（t.Logf 原文）
	Lines    int       `json:"lines"`
}

// CaseCounts 模块短名 → 顶层用例数（覆盖现状.md 用这个口径）。
func (s *Summary) CaseCounts() map[string]int {
	out := make(map[string]int, len(s.Packages))
	for _, p := range s.Packages {
		out[p.Short] = p.Cases
	}
	return out
}

// Summarize 解析 `go test -json` 的 JSONL 流。
//
// 统计口径（与"用例数"一致）：
//   - **只有顶层用例计数**（Test 不含 "/"）：子测试是同一个用例内的分支，不重复计；
//   - 一个包只在有 Test 事件时才出现在结果里（没有用例的包如 test/testsupport 不计）；
//   - 耗时取包级事件的 Elapsed（拿不到就取用例耗时之和）。
func Summarize(r io.Reader) (*Summary, error) {
	sum := &Summary{Packages: []Package{}, Failures: []Failure{}, Gate: []string{}}
	idx := map[string]int{}            // 包全名 → Packages 下标
	caseAction := map[string]string{}  // 包+用例 → pass/fail/skip（子测试不进来）
	firstLine := map[string]string{}   // 包+用例 → 首个有信息量的输出行
	pkgElapsed := map[string]float64{} // 包全名 → 耗时

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // 单行可能很长（失败用例打印大对象）
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue // go test 偶发非 JSON 行（如构建错误），忽略
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		sum.Lines++
		if ev.Package == "" {
			continue
		}
		short := packageShort(ev.Package)

		// 闸门输出：test/chaindata 的 t.Logf（json 里带 "文件:行: " 前缀，去前缀后是 "[链id] …"）
		if short == "test/chaindata" && ev.Output != "" {
			if t := stripLogPrefix(ev.Output); strings.HasPrefix(t, "[") {
				sum.Gate = append(sum.Gate, t)
			}
		}

		key := ev.Package + "\x00" + ev.Test
		// 用例输出：记下首个有信息量的行（失败时报告要列它）
		if ev.Test != "" && ev.Output != "" && firstLine[key] == "" {
			firstLine[key] = trimRunnerMarkers(ev.Output)
		}

		if ev.Test == "" {
			if ev.Elapsed > 0 {
				pkgElapsed[ev.Package] = ev.Elapsed
			}
			continue
		}
		if strings.Contains(ev.Test, "/") {
			continue // 子测试：不单独计数
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			if _, ok := idx[ev.Package]; !ok {
				idx[ev.Package] = len(sum.Packages)
				sum.Packages = append(sum.Packages, Package{Name: ev.Package, Short: short})
			}
			caseAction[key] = ev.Action
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// 结算：每个用例只算一次；失败清单附首行断言信息
	for key, action := range caseAction {
		parts := strings.SplitN(key, "\x00", 2)
		pkgName, testName := parts[0], parts[1]
		i, ok := idx[pkgName]
		if !ok {
			continue
		}
		p := &sum.Packages[i]
		p.Cases++
		sum.Cases++
		switch action {
		case "fail":
			p.Failed++
			sum.Failed++
			sum.Failures = append(sum.Failures, Failure{
				Package: p.Short, Test: testName, FirstLine: firstLine[key],
			})
		case "skip":
			p.Skipped++
			sum.Skipped++
		default:
			p.Passed++
			sum.Passed++
		}
	}
	for i := range sum.Packages {
		sum.Packages[i].Elapsed = pkgElapsed[sum.Packages[i].Name]
		sum.Elapsed += sum.Packages[i].Elapsed
	}

	sort.Slice(sum.Packages, func(i, j int) bool { return sum.Packages[i].Short < sum.Packages[j].Short })
	sort.Slice(sum.Failures, func(i, j int) bool {
		if sum.Failures[i].Package != sum.Failures[j].Package {
			return sum.Failures[i].Package < sum.Failures[j].Package
		}
		return sum.Failures[i].Test < sum.Failures[j].Test
	})
	return sum, nil
}

// packageShort 包全名 → 短名（zyctrlcenter/test/api → test/api）。
func packageShort(pkg string) string {
	if i := strings.Index(pkg, "/test/"); i >= 0 {
		return "test/" + pkg[i+len("/test/"):]
	}
	return pkg
}

// stripLogPrefix 去掉 `go test -json` 给 t.Logf 加的 "文件:行: " 前缀
// （形如 "    chaindata_test.go:294: [newbie_full] …" → "[newbie_full] …"）。
func stripLogPrefix(out string) string {
	s := strings.TrimSpace(out)
	if i := strings.Index(s, ".go:"); i >= 0 {
		if j := strings.Index(s[i:], ": "); j >= 0 {
			return strings.TrimSpace(s[i+j+2:])
		}
	}
	return s
}

// trimRunnerMarkers 去掉 go test 的运行标记行（=== RUN / --- FAIL / 缩进等），取首行有信息量的输出。
func trimRunnerMarkers(out string) string {
	t := strings.TrimSpace(out)
	if t == "" {
		return ""
	}
	for _, prefix := range []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- FAIL", "--- PASS", "--- SKIP", "FAIL", "ok ", "?"} {
		if strings.HasPrefix(t, prefix) {
			return ""
		}
	}
	return strings.ReplaceAll(t, "\t", " ")
}

// SortedGate 闸门结论按原文排序（同一批数据每次输出顺序稳定）。
func (s *Summary) SortedGate() []string {
	out := append([]string(nil), s.Gate...)
	sort.Strings(out)
	return out
}

// FailFirstLines 失败用例 → 首行断言信息（报告"失败项"表用）。
func (s *Summary) FailFirstLines() map[string]string {
	out := make(map[string]string, len(s.Failures))
	for _, f := range s.Failures {
		out[f.Package+"::"+f.Test] = f.FirstLine
	}
	return out
}

// String 一行摘要（工具打印用）。
func (s *Summary) String() string {
	return fmt.Sprintf("用例 %d：通过 %d / 失败 %d / 跳过 %d（%d 个测试包）",
		s.Cases, s.Passed, s.Failed, s.Skipped, len(s.Packages))
}
