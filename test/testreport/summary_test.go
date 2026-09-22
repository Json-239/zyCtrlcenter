// testreport 模块测试：`go test -json` 的统计口径（只数顶层用例、失败取首行断言信息、
// 抽取链数据闸门结论）+ 覆盖现状.md 的数字校验/回写。
//
// 这些用例的意义：报告工具与文档数字都只能来自这里定义的口径 —— 口径错了，报告就会骗人。
package testreport_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"zyctrlcenter/internal/testreport"
	"zyctrlcenter/test/testsupport"
)

// 样例 JSONL：形状照 `go test -json` 真实输出（含子测试、包级事件、失败用例输出、闸门 t.Logf）。
const sampleJSONL = `{"Time":"2026-09-21T10:00:00Z","Action":"start","Package":"zyctrlcenter/test/api"}
{"Time":"2026-09-21T10:00:00Z","Action":"run","Package":"zyctrlcenter/test/api","Test":"TestStartAutoGhostCarriesNavPayload"}
{"Time":"2026-09-21T10:00:00Z","Action":"output","Package":"zyctrlcenter/test/api","Test":"TestStartAutoGhostCarriesNavPayload","Output":"=== RUN   TestStartAutoGhostCarriesNavPayload\n"}
{"Time":"2026-09-21T10:00:00Z","Action":"output","Package":"zyctrlcenter/test/api","Test":"TestStartAutoGhostCarriesNavPayload","Output":"=== PAUSE TestStartAutoGhostCarriesNavPayload\n"}
{"Time":"2026-09-21T10:00:00Z","Action":"output","Package":"zyctrlcenter/test/api","Test":"TestStartAutoGhostCarriesNavPayload","Output":"    start_ghost_test.go:61: 抓鬼必须带 chain 载荷（不带=机器人原地不动）: map[]\n"}
{"Time":"2026-09-21T10:00:00Z","Action":"fail","Package":"zyctrlcenter/test/api","Test":"TestStartAutoGhostCarriesNavPayload","Elapsed":0.01}
{"Time":"2026-09-21T10:00:00Z","Action":"run","Package":"zyctrlcenter/test/api","Test":"TestStartRejectsNavOnlyChain"}
{"Time":"2026-09-21T10:00:01Z","Action":"pass","Package":"zyctrlcenter/test/api","Test":"TestStartRejectsNavOnlyChain","Elapsed":0.02}
{"Time":"2026-09-21T10:00:01Z","Action":"run","Package":"zyctrlcenter/test/api","Test":"TestStartRejectsNavOnlyChain/sub"}
{"Time":"2026-09-21T10:00:01Z","Action":"pass","Package":"zyctrlcenter/test/api","Test":"TestStartRejectsNavOnlyChain/sub","Elapsed":0}
{"Time":"2026-09-21T10:00:01Z","Action":"output","Package":"zyctrlcenter/test/api","Test":"TestStartRejectsNavOnlyChain","Output":"PASS\n"}
{"Time":"2026-09-21T10:00:01Z","Action":"pass","Package":"zyctrlcenter/test/api","Elapsed":0.03}
{"Time":"2026-09-21T10:00:01Z","Action":"start","Package":"zyctrlcenter/test/chaindata"}
{"Time":"2026-09-21T10:00:02Z","Action":"output","Package":"zyctrlcenter/test/chaindata","Test":"TestTaskNpcPositionsAreWalkable","Output":"    chaindata_test.go:294: [newbie_full] 任务 NPC 可达性：引用 32 个，全部通过\n"}
{"Time":"2026-09-21T10:00:02Z","Action":"output","Package":"zyctrlcenter/test/chaindata","Test":"TestGhostLandingPointsAreWalkable","Output":"    chaindata_test.go:379: [zhongkui_nav] 刷鬼图 [9 10 11] · 落点 7 个（已知阻挡 0）：可达性检查完成\n"}
{"Time":"2026-09-21T10:00:02Z","Action":"pass","Package":"zyctrlcenter/test/chaindata","Test":"TestTaskNpcPositionsAreWalkable","Elapsed":0.30}
{"Time":"2026-09-21T10:00:02Z","Action":"pass","Package":"zyctrlcenter/test/chaindata","Test":"TestGhostLandingPointsAreWalkable","Elapsed":0.10}
{"Time":"2026-09-21T10:00:02Z","Action":"pass","Package":"zyctrlcenter/test/chaindata","Elapsed":0.40}
not-a-json-line
{"Time":"2026-09-21T10:00:03Z","Action":"skip","Package":"zyctrlcenter/test/wsutil","Test":"TestHandshake","Elapsed":0}
`

func TestSummarizeParsesGoTestJSON(t *testing.T) {
	sum, err := testreport.Summarize(strings.NewReader(sampleJSONL))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 顶层用例才算：api 2 个（失败 1 通过 1），子测试不计；chaindata 2 个；wsutil 1 个跳过
	if sum.Cases != 5 || sum.Passed != 3 || sum.Failed != 1 || sum.Skipped != 1 {
		t.Fatalf("统计口径不对（应 5/3/1/1）: %+v", sum)
	}
	counts := sum.CaseCounts()
	if counts["test/api"] != 2 || counts["test/chaindata"] != 2 || counts["test/wsutil"] != 1 {
		t.Fatalf("每个包的用例数不对: %v", counts)
	}
	if _, ok := counts["test/testsupport"]; ok {
		t.Fatalf("没跑用例的包不该出现在结果里: %v", counts)
	}
	// 失败项：要带首行断言信息（去掉 === RUN / --- FAIL 这类运行标记）
	if len(sum.Failures) != 1 {
		t.Fatalf("应记录 1 条失败: %+v", sum.Failures)
	}
	f := sum.Failures[0]
	if f.Package != "test/api" || f.Test != "TestStartAutoGhostCarriesNavPayload" ||
		!strings.Contains(f.FirstLine, "抓鬼必须带 chain 载荷") {
		t.Fatalf("失败项应带首行断言信息: %+v", f)
	}
	// 闸门：test/chaindata 的 t.Logf 结论要抽出来（报告"关键保障"节直接用）
	if len(sum.Gate) != 2 {
		t.Fatalf("应抽出 2 条闸门结论: %v", sum.Gate)
	}
	for _, g := range sum.Gate {
		if !strings.HasPrefix(g, "[") {
			t.Fatalf("闸门结论应是 [链id] 开头的原文: %q", g)
		}
	}
	// 包耗时取包级事件
	for _, p := range sum.Packages {
		if p.Short == "test/api" && p.Elapsed != 0.03 {
			t.Fatalf("包耗时应取包级事件: %+v", p)
		}
	}
}

func TestSummarizeEmptyInputIsZero(t *testing.T) {
	sum, err := testreport.Summarize(strings.NewReader(""))
	if err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
	if sum.Cases != 0 || len(sum.Packages) != 0 {
		t.Fatalf("空输入应得零: %+v", sum)
	}
}

func TestReportMarkdownHasFourSections(t *testing.T) {
	sum, _ := testreport.Summarize(strings.NewReader(sampleJSONL))
	rep := testreport.Report{
		Date: "2026-09-21", OK: false, Vet: "ok", Race: "skipped",
		Totals: testreport.Totals{Cases: sum.Cases, Passed: sum.Passed, Failed: sum.Failed, Skipped: sum.Skipped},
		Packages: sum.Packages, Failures: sum.Failures, Gate: sum.SortedGate(),
		Env: testreport.Env{OS: "windows amd64", Go: "go1.24.3"},
		Cmd: "go run ./tools/test_report",
	}
	md := rep.RenderMarkdown()
	for _, want := range []string{
		"# 测试报告 2026-09-21", "## 1. 汇总", "## 2. 关键保障（断言级）",
		"## 3. 失败项", "## 4. 复现方式", "机器人侧实收命令", "[newbie_full] 任务 NPC 可达性",
		"抓鬼必须带 chain 载荷", "run_tests.bat",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("报告缺少内容 %q:\n%s", want, md)
		}
	}
	if !strings.Contains(md, "**有失败**") {
		t.Fatalf("有失败时要显眼：%s", md)
	}
}

func TestReportMarkdownSaysGateNotRun(t *testing.T) {
	rep := testreport.Report{Date: "2026-09-21", OK: true, Vet: "ok", Race: "skipped"}
	if !strings.Contains(rep.RenderMarkdown(), "本次未跑") {
		t.Fatal("没有闸门结论时要写清「本次未跑」（不能看起来像「检查过没问题」）")
	}
}

// ---------------- 覆盖现状.md 的数字 ----------------

const sampleDoc = "> 数据更新于 2026-09-19。\n\n" +
	"| 模块 | 用例数 | 说明 | 详细 |\n|---|---|---|---|\n" +
	"| `test/api` | 51 | 路由与下发内容 | [README](../../test/api/README.md) |\n" +
	"| `test/chaindata` | 4 | 客观闸门 | [README](../../test/chaindata/README.md) |\n" +
	"| **合计** | **217** | 全量秒级完成 | — |\n"

func TestParseCoverageTableAndValidateDrift(t *testing.T) {
	cov, err := testreport.ParseCoverageTable(sampleDoc)
	if err != nil {
		t.Fatalf("解析表格失败: %v", err)
	}
	if len(cov.Rows) != 3 || cov.Total() != 217 || cov.SumRows() != 55 {
		t.Fatalf("解析结果不对: rows=%d total=%d sum=%d", len(cov.Rows), cov.Total(), cov.SumRows())
	}
	// 实测：api 57、chaindata 5 → 三处不一致（api 行、chaindata 行、合计）
	diffs := testreport.ValidateCoverage(cov, map[string]int{"test/api": 57, "test/chaindata": 5})
	if len(diffs) != 3 {
		t.Fatalf("应报出 3 处差异: %+v", diffs)
	}
	joined := ""
	for _, d := range diffs {
		joined += d.String() + "\n"
	}
	for _, want := range []string{"test/api：文档 51 → 实测 57", "test/chaindata：文档 4 → 实测 5", "合计：文档 217 → 实测 62"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("差异清单缺少 %q:\n%s", want, joined)
		}
	}
	// 文档里有、实测没有（missing）与合计差异都要能报；实测有、文档没列（unlisted）同理
	more := testreport.ValidateCoverage(cov, map[string]int{"test/api": 51})
	if len(more) != 2 {
		t.Fatalf("应报 missing 与合计差异: %+v", more)
	}
	diffsUnlisted := testreport.ValidateCoverage(cov, map[string]int{"test/api": 51, "test/chaindata": 4, "test/newmod": 3})
	found := false
	for _, d := range diffsUnlisted {
		if d.Kind == "unlisted" && d.Module == "test/newmod" {
			found = true
		}
	}
	if !found {
		t.Fatalf("实测有、文档没登记的包要报 unlisted: %+v", diffsUnlisted)
	}
}

func TestRewriteCoverageTouchesOnlyCountColumn(t *testing.T) {
	cov, _ := testreport.ParseCoverageTable(sampleDoc)
	next, diffs := testreport.RewriteCoverage(sampleDoc, cov, map[string]int{"test/api": 57, "test/chaindata": 5})
	if len(diffs) != 3 {
		t.Fatalf("应报 3 处改动: %+v", diffs)
	}
	// 数字变了
	for _, want := range []string{"| `test/api` | 57 | 路由与下发内容 |", "| `test/chaindata` | 5 | 客观闸门 |", "| **合计** | **62** |"} {
		if !strings.Contains(next, want) {
			t.Fatalf("回写后缺少 %q:\n%s", want, next)
		}
	}
	// 说明/链接/版本说明等人工内容不许动
	for _, want := range []string{"数据更新于 2026-09-19", "路由与下发内容", "../../test/api/README.md", "全量秒级完成"} {
		if !strings.Contains(next, want) {
			t.Fatalf("回写改了不该改的内容 %q:\n%s", want, next)
		}
	}
	// 回写后再校验应干净
	cov2, err := testreport.ParseCoverageTable(next)
	if err != nil {
		t.Fatalf("回写后解析失败: %v", err)
	}
	if d := testreport.ValidateCoverage(cov2, map[string]int{"test/api": 57, "test/chaindata": 5}); len(d) != 0 {
		t.Fatalf("回写后应无差异: %+v", d)
	}
}

// 真实文档的自洽性：合计 == 各行相加，且每行都能在仓库里找到对应测试目录。
// （数字与实测的对比由 tools/test_report 负责——测试里不跑 go test，避免递归。）
func TestCoverageDocIsSelfConsistent(t *testing.T) {
	root := testsupport.TestRoot(t) // <repo>/test
	docPath := filepath.Join(root, "..", "docs", "04-测试", "覆盖现状.md")
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", docPath, err)
	}
	cov, err := testreport.ParseCoverageTable(string(raw))
	if err != nil {
		t.Fatalf("解析覆盖现状总览表失败: %v", err)
	}
	if cov.TotalIdx < 0 {
		t.Fatal("总览表应有「合计」行")
	}
	if got, want := cov.Total(), cov.SumRows(); got != want {
		t.Fatalf("合计与各行相加不一致（%d != %d）—— 数字漂了，跑 go run ./tools/test_report -write 修", got, want)
	}
	for _, r := range cov.Rows {
		if r.IsTotal {
			continue
		}
		dir := filepath.Join(root, "..", filepath.FromSlash(r.Module))
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Fatalf("总览表里的 %s 在仓库里找不到对应目录: %v", r.Module, err)
		}
	}
}

// 每个"有用例的测试包"都必须在总览表里登记（新增模块忘了登记 → 这里红）。
func TestCoverageDocListsEveryTestPackage(t *testing.T) {
	root := testsupport.TestRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "..", "docs", "04-测试", "覆盖现状.md"))
	if err != nil {
		t.Fatal(err)
	}
	cov, err := testreport.ParseCoverageTable(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, r := range cov.Rows {
		if !r.IsTotal {
			listed[r.Module] = true
		}
	}

	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	testFuncRe := regexp.MustCompile(`(?m)^func Test[A-Z]`)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(root, d.Name(), "*_test.go"))
		hasCase := false
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err == nil && testFuncRe.Match(body) {
				hasCase = true
				break
			}
		}
		if !hasCase {
			continue // 夹具目录 / 只有辅助函数的包（如 testsupport）
		}
		if !listed["test/"+d.Name()] {
			t.Fatalf("test/%s 有用例但没登记进 覆盖现状.md 总览表（新增测试模块要补一行）", d.Name())
		}
	}
}
