# event 测试说明

## 被测对象
- 模块：`internal/services/event/event.go`
- 关键函数：`HandleEvent`（表驱动） / `HandledTypes` / `AfterEvent` / `RunStatusPoller`
- 处理语义（状态映射/记账/下机）见 [docs/02-架构/事件与数据流.md](../../docs/02-架构/事件与数据流.md) §2-§3

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `event_test.go` | 状态迁移、错误记账、下机（开/关自动下机）、广播口径、协议覆盖、panic 兜底 |

## 前置条件
- 输入来自 `test/fixtures/events/*.json`（真实抓包 + 显式构造，规则见 [夹具规范](../../docs/04-测试/夹具规范.md)）。
- 临时数据目录（`t.TempDir()`）；控制通道未启动（下发必然失败，不影响状态断言）。

## 运行方式
- 单项：`go test ./test/event/ -run TestErrorFixtureRecordsCodeRepeatAndFailedList -v`
- 模块级：`go test ./test/event/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestTaskProgressFixtureUpdatesStateAndLog` | 真实 task_progress | 任务号/done 落状态 + 落运行历史 |
| `TestRobotStateFixtureUpdatesDialogAndMap` | 真实 robot_state | 状态 DIALOG、地图/坐标正确 |
| `TestErrorFixtureRecordsCodeRepeatAndFailedList` | 真实 TASK_STUCK 两次 | err_repeat 累计、进入「任务失败待处理」清单、落盘 |
| `TestStuckCodeCountsAndTriggersReghost` | STUCK_WAIT_NEXT / STUCK_CLICK / NO_LEGAL_ROUTE | 卡死前缀码计当日 churn 次数 + 触发 reghost（reason=原文）；普通码两者都不做 |
| `TestGhostOfflineAutoRemoveWhenEnabled` | ghost_offline（自动下机开） | 标记移除 + 删行 + 落历史 + 心跳不复活 |
| `TestGhostOfflineKeepsRowWhenAutoRemoveDisabled` | ghost_offline（自动下机关） | 不改状态、不删行，但历史必写 |
| `TestChainDoneStateVisibleWhenNotAutoRemove` | 链完成（关自动下机） | DONE/chain_done 可见 |
| `TestChainDoneAutoRemoveDropsRow` | 链完成（开自动下机） | 标记移除 + 删行 + 下机历史落盘 |
| `TestChainDoneAlreadyDoneStaysOnline` | already_done | 不下机、保留行 |
| `TestStatusReplyUpdatesRobotsAndServer` | 构造 status_reply（2 机器人） | 服务器地址/字段映射/hs/err_code 保留 |
| `TestUnknownEventOnlyLogged` | 未知事件 | 不改状态、只落历史（fail-loud） |
| `TestRobotPosBatchFlushAndState` | 两条 robot_pos | 状态立即更新 + 100ms 合并推送 `robot_pos_batch` |
| `TestDebugLogNotBroadcast` | debug 日志 / 普通事件 / robot_pos | 只落盘不推 WS / 推 / 不逐条推 |
| `TestEventTableCoversProtocolSpec` | 协议表覆盖 | 协议文档里每个事件都有处理分支 |
| `TestHandlerRecoversFromPanic` | 字段类型不符 | 处理器容错不 panic |

## 已知限制
- 未覆盖 `RunStatusPoller` 的时间行为（3 秒轮询/10 分钟清理阈值，用例待补虚拟时钟）。
