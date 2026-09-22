# test/autotask：定时自动任务（新手链 / 抓鬼 两套独立策略）

## 被测对象
- 模块：`internal/services/autotask/autotask.go`（引擎：纯决策 + 注入依赖）
- 壳层（候选判定/上线/注册/下发）：`internal/api/autotask.go`（接口用例见 `test/api/autotask_test.go`）
- 口径对齐参考实现 `robot/ctrlcenter/routers/auto_onboard.py`（每 N 分钟取号上线 + 启动任务；没号可注册）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `autotask_test.go` | 随机批量、**按保持数补差额**、离线先上线再延迟下发、硬上限、没号自动注册、停止撤销待下发、两策略互不影响、间隔抖动 |

## 前置条件
- 全部是**纯逻辑**：假时钟 + 假 Rand + 假依赖，不 sleep、不起 goroutine、不联网。

## 运行方式
- 单项：`go test ./test/autotask/ -run TestOfflineCandidateOnlineThenDelayedLaunch -v`
- 模块级：`go test ./test/autotask/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestLaunchOnlineCandidateImmediately` | 候选都已在线 | 直接下发、不用上线；累计 Picked；间隔没到不再跑 |
| `TestOfflineCandidateOnlineThenDelayedLaunch` | 候选离线 | 先上线；**8 秒后**才下发（等角色数据就绪） |
| `TestMaxOnlineBlocks` | 同时在线上限已到 | 不拉、不报错；15 秒后再看；空出槽位恢复 |
| `TestRegisterWhenNoCandidates` | 没号可挑 + 开了自动注册 | 按配置个数发起注册并记录；没开就什么都不做 |
| `TestStopCancelsPendingLaunch` | 停止策略 | 不再拉起，且撤销"等下发"队列 |
| `TestKindsAreIndependent` | 停新手链 | 抓鬼照常跑（两套互不影响） |
| `TestIntervalJitter` | 间隔抖动 | 实际间隔 = 基础 + rand(抖动) |
| `TestTargetOnlineRefillsOnlyDeficit` | **保持在线数**（按目标补差额） | 在跑 47 / 目标 50 → 本轮只补 3 个；达标后不再拉起 |

## 已知限制
- 不测真实上线/注册/下发（那是壳层与机器人的事，由 `test/api/autotask_test.go` 与 `test/ctrl` 覆盖）。
- 策略配置不落盘：中控重启后需重新启动定时任务（与参考实现一致）。
