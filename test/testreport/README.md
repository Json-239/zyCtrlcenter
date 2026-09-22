# test/testreport：测试报告与文档数字 覆盖 `internal/testreport`（`tools/test_report` 的纯函数部分）。

## 被测对象

- 模块：`internal/testreport/summary.go`（`go test -json` 解析与统计）、`coverage.go`（覆盖现状.md 的用例数列）、`report.go`（报告结构与 Markdown 渲染）
- 关键函数：`Summarize` / `Summary.CaseCounts` / `ParseCoverageTable` / `ValidateCoverage` / `RewriteCoverage` / `Report.RenderMarkdown`
- 工具入口：`tools/test_report`（跑 vet + 全量测试 → 写 `docs/04-测试/报告-YYYYMMDD.md` 与 `data/test_report.json`）

## 测试文件

| 文件 | 覆盖场景 |
|---|---|
| `summary_test.go` | 样例 JSONL 的统计口径（只数顶层用例、失败首行、闸门抽取、包耗时）、空输入、报告 Markdown 四节、覆盖现状表的解析/差异/回写、真实文档自洽性 |

## 前置条件

- 全部输入都是**内联样例字符串**，不跑 `go test`、不联网、不写项目文件（回写类用例只在内存里的字符串上做）。
- `TestCoverageDocIsSelfConsistent` / `TestCoverageDocListsEveryTestPackage` 会**只读**读项目 `docs/04-测试/覆盖现状.md` 与 `test/` 目录：数字漂了/新模块忘登记就失败。

## 运行方式

- 单项：`go test ./test/testreport/ -run TestCoverageDocIsSelfConsistent -v`
- 模块级：`go test ./test/testreport/ -v`
- 端到端（跑全量测试并写报告）：`go run ./tools/test_report`（数字与实测不一致 → 非零退出；`-write` 回写）

## 覆盖场景清单

| 用例 | 场景 | 预期 |
|---|---|---|
| `TestSummarizeParsesGoTestJSON` | 顶层用例 + 子测试 + 包级事件 + 失败输出 + 闸门 t.Logf | 用例 5/通过 3/失败 1/跳过 1；子测试不计数；失败项带首行断言信息；抽出 2 条闸门结论 |
| `TestSummarizeEmptyInputIsZero` | 空输入 | 零结果、不报错 |
| `TestReportMarkdownHasFourSections` | 有失败的报告 | 四节齐全、失败项与闸门都在、结论显眼 |
| `TestReportMarkdownSaysGateNotRun` | 没有闸门结论（本机没跑客观校验） | 明确写「本次未跑」，不能看起来像检查过没问题 |
| `TestParseCoverageTableAndValidateDrift` | 文档数字与实测不一致 | 逐行报「文档 51 → 实测 57」；`missing`（文档有、实测无）与 `unlisted`（实测有、文档没列）都能报 |
| `TestRewriteCoverageTouchesOnlyCountColumn` | `-write` 回写 | 只改「用例数」列与合计行；说明/链接/版本行原样；回写后再校验无差异 |
| `TestCoverageDocIsSelfConsistent` | 真实 `覆盖现状.md` | 合计 == 各行相加；每行模块目录存在 |
| `TestCoverageDocListsEveryTestPackage` | 真实 `test/` 目录 | 每个含 `func Test*` 的测试包都登记在总览表里 |

## 已知限制

- 统计口径只到**顶层用例**（子测试不单独计数）——这是"用例数"的既定口径，与 `覆盖现状.md` 一致。
- 闸门结论按 `test/chaindata` 里 `[` 开头的 `t.Logf` 原文抽取；改写那些日志的措辞会反映到报告里（不会静默丢，报告会少一行）。
- 差异清单不做自动修复之外的判断（例如"少登记了包"需要人补一行说明列，工具不猜文案）。
