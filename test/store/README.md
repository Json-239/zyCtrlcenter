# store 测试说明

## 被测对象
- 模块：`internal/store/store.go`
- 关键函数：`LogEvent` / `ReadTail` / `ReadBotLogs` / `CleanupOldRuns` / `CleanupBotLogs` / `ClearAll` / `CurrentPath`

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `store_test.go` | 日期级路径、尾部回读、单 bot 分流、保留窗口清理、清空、超限熔断归档 |

## 前置条件
- 数据隔离：全部使用 `t.TempDir()`，不碰真实 `data/`。
- 不联网；熔断用例有 1.1s 等待（越过 1s 检查节流）。

## 运行方式
- 单项：`go test ./test/store/ -run TestCurrentPathIsDateBased -v`
- 模块级：`go test ./test/store/ -v`
- 全量：`run_tests.bat`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestCurrentPathIsDateBased` | 当天文件路径 | `runs_YYYYMMDD.jsonl`（日期级） |
| `TestLogEventWritesAndReadsTail` | 写 5 条读尾部 3 条 | 返回最后 3 条且顺序正确、ts 自动补 |
| `TestBotLogsSeparatedByAccount` | 多账号事件 | 单 bot 日志按日期分流且不混号 |
| `TestCleanupOldRunsKeepsTodayAndRecent` | 40 天前/3 天前/当天 | 只删 40 天前，保留其余 |
| `TestClearAllTruncatesTodayAndRemovesHistory` | 清空日志 | 历史 runs 删除、当天截断、bot 目录删除 |
| `TestRotationArchivesOversizedFile` | 单文件超 1MB | 归档为 `.over_*`（只留最新），新文件从空开始 |

## 已知限制
- 未覆盖跨天自动切换的真实时钟场景（用文件时间戳模拟替代）。
