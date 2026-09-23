# test/roampool：游荡池 keeper（任务池缺人 → 立刻回收；余量 → 按图均匀派游荡）

## 被测对象
- 模块：`internal/services/roampool/roampool.go`
- 关键函数：
  - 纯函数（可直接单测）：`Roaming`（是否在游荡）、`Idle`（是否空闲）、`PickReclaim`（按图人数降序挑游荡号）、
    `PickIdle`（挑空闲号）、`BalanceAssign`（按图均匀分配）、`MapLoads`（各图当前人数）
  - 决策与状态：`Keeper.Tick`（一轮：回收优先 → 补位）、`Keeper.Status`、`Keeper.SetConfig` / `Load`
- 壳层（不在本模块用例范围）：`internal/api/roampool.go`（取数/缺口/图/下发，复用 `DispatchRoam`/`StopRoam`）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `roampool_test.go` | 纯函数边界（在游荡/空闲判据、回收优先级、挑空闲、均匀分配）+ 一轮决策（禁用/回收优先/限幅/无游荡号/按图补位/随机图/达标/单图失败/白名单交集）+ 参数默认值·落盘·环境变量覆盖·范围校验 |

## 前置条件
- 全部用**假依赖**（`Deps` 注入）：假机器人快照、假缺口、假图列表、假下发/停止（只记下发内容）。
  **不联网、不起进程、不碰真实 `data/`**（仅用 `t.TempDir()` 验证参数落盘）。
- 输入是构造的结构体/数字（不是报文），依据是 2026-09-22 拍板口径：
  在线总数 200 = 抓鬼池 100 + 新手池 0 + 游荡池（余量，目标 100 左右）；任务缺人**立刻回收**；游荡**按图均匀**。
- 空闲判据复用 `waterline.Busy`（与在线水位保持器同一口径，避免两套判据打架）；
  孵化的号**不算游荡池**（它是自动任务自己拉起的有时长会话，回收会打断孵化）。

## 运行方式
- 单项：`go test ./test/roampool/ -run TestPickReclaimPrefersCrowdedMap -v`
- 模块级：`go test ./test/roampool/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestRoamingAndIdleJudgements` | walk.enabled=true / 孵化中 / 普通在线 / 离线；NAV / WAIT_TASK(有任务) / SUBMIT / ERROR / WAIT_GHOST(无会话) | 只有"游荡且不在孵化"算在游荡；在忙/异常的都不算空闲（SUBMIT/ERROR 2026-09-23 补齐），等待段 WAIT_GHOST 仍算空闲 |
| `TestPickReclaimPrefersCrowdedMap` | 图10 挤 3 个（2 个在游荡）、图26/24 各 1 个 | 先回收图10 的游荡号（顺带纠偏分布） |
| `TestPickReclaimShortfallAndEmpty` | 游荡号不足 n / 没有游荡号 / n=0 | 有几个给几个；空结果；空结果 |
| `TestPickReclaimTieBreakIsStable` | 两图人数相同 | 按账号升序（结果稳定、日志可预期） |
| `TestPickIdleFiltersBusyAndOffline` | 空闲 / 抓鬼 / 战斗 / 游荡 / 孵化 / WAIT_TASK(有任务) / SUBMIT / ERROR / 离线 / 空账号 | 只挑出真正空闲的号（SUBMIT/ERROR 不派游荡） |
| `TestPickIdleRespectsN` | 3 个空闲要 2 个 / 要 0 个 | 保序取 2；n≤0 空结果 |
| `TestBalanceAssignEven` | 3 张空图 6 个号 | 各 2 个 |
| `TestBalanceAssignRemainder` | 3 张空图 5 个号 | 2/2/1（余数给并列中图号小的） |
| `TestBalanceAssignMoreMapsThanAccounts` | 5 张图 2 个号 | 只用图号最小的 2 张图，各 1 个 |
| `TestBalanceAssignRespectsExistingLoad` | 图10 已 5 人、图24/26 各 1 人，4 个号 | 全给人少的图（派完 5/3/3，最大人数不升） |
| `TestBalanceAssignEdges` | 没号 / 没图 / n=0 | 一律空结果 |
| `TestMapLoadsCountsOnlinePerMap` | 3 张候选图 + 抓鬼号 | 按图统计在线数、图号升序、没人的图 0 |
| `TestTickDoesNothingWhenDisabled` | enabled=false 且缺口 3 | 一次动作都不发 |
| `TestTickReclaimsBeforeDispatching` | 缺口 3、在游荡 2、有 2 个空闲号 | 两个游荡号都回收，**同轮不补位**（名额让给任务池） |
| `TestTickReclaimRespectsMaxStep` | 缺口 10、在游荡 4、max_step=2 | 单轮最多回收 2 个 |
| `TestTickDeficitWithNoRoamersDoesNotDispatch` | 缺口 2、没有游荡号 | 不回收也不补位（避免"补了又抢"），动作文案说明原因 |
| `TestTickDispatchBalancedByMapLoad` | 4 个空闲、图10 已 4 人、图26 已 2 人、目标 4 | 图26 分到 3、图10 分到 1；档位/限时按配置下发 |
| `TestTickDispatchBalancedTieBreakByMapID` | 图10 只 1 人、图26 已 4 人、目标 5 | 图10×4 / 图26×1（并列时图号小的先派） |
| `TestTickDispatchRandomWhenBalanceOff` | balance=false | 一条命令下发 `mapid="random"`（每号自抽） |
| `TestTickStopsDispatchingAtTarget` | 在游荡 2 / 目标 2 | 不动作，文案说明已达标 |
| `TestTickSingleMapFailureDoesNotAbortOthers` | 图10 下发失败、图26 成功 | 图26 照常下发；失败写日志；动作文案只算真的发成功的图（不虚报） |
| `TestTickWhitelistIntersectsGridMaps` | 白名单 [99, 26]，有网格的图 [10, 26] | 只派到 26（没有网格的 99 不派） |
| `TestConfigSaveLoadRoundTrip` | 设置参数 → 新建 Keeper 读回 | enabled/target/maps/balance 原样读回 |
| `TestConfigEnvOverridesFile` | `CTRL_ROAMPOOL_*` 覆盖文件 | env 优先；`EnvPinned` 报告被固定的字段 |
| `TestConfigValidateRejectsOutOfRange` | target<0 / 间隔过小 / max_step=0 / minutes<0 / 图号非法 | 一律拒绝且不生效（内存参数不变） |

## 已知限制
- 只对**当前区**生效（壳层传入的机器人快照已按当前区过滤）。
- 不做"在途记账"：下发后等机器人上报 `walk.enabled=true`（秒级）才计入在游荡；
  机器人端已有"空闲 90s 自动游荡"兜底，所以本模块只保证**名额与分布**、不抢时间。
- 回收会打断正在进行的游荡（可能有路程损耗）——这是用户拍板的口径（任务优先）。
