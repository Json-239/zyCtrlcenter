# test/waterline：在线水位保持器（当前区在线人数维持在目标附近）

## 被测对象
- 模块：`internal/services/waterline/waterline.go`
- 关键函数：
  - 纯函数（可直接单测）：`ComputeAdjust`（死区/限幅）、`PickOffline`（空闲优先 / 忙号进待下线）、
    `PickOnlineCandidates`（可用性过滤）、`Busy`（不硬断判据）
  - 决策与状态：`Keeper.Tick`（一轮：取数 → 算调整 → 执行）、`Keeper.Status`、`Keeper.SetConfig` / `Load`
- 壳层（不在本模块用例范围）：`internal/api/waterline.go`（取数/候选/上下线，复用批量上下线通路）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `waterline_test.go` | 纯决策 4 个函数的边界；一轮决策（禁用/取数口径/死区/补号/压号/待下线/保底/失败不崩）；参数默认值、落盘与范围校验 |

## 前置条件
- 全部用**假依赖**（`Deps` 注入）：假时钟、假机器人快照、假账号池候选、假上下线（只记下发内容）。
  **不联网、不起进程、不碰真实 `data/`**（仅用 `t.TempDir()` 验证参数落盘）。
- 输入是构造的结构体/数字（不是报文），依据是 2026-09-22 拍板口径：
  人数 = 服务端全服在线（含真人，`svr_online`），缺失/超过 180 秒过期时用本地握手数兜底；
  每轮 60s、死区 3、单轮 ±5、空闲优先断号。
  2026-09-23 起 `Busy`（"不该被自动打扰"判据）与前端地图页 26f4660 同口径：SUBMIT（交付中）=
  推进中、ERROR（卡住/停链）= 异常 —— 两者都不算空闲（不硬压、不派活；ERROR 的号进待下线后
  要等它自愈回正常态才按常规处理）。
- 假 `Offline` 会像真实壳层一样把号标记为离线（壳层会 `MarkRemoved` + 删行），
  假 `Online` **不**立刻上线（真实要等机器人登录上报）——在途记账正是靠这个差异验证。

## 运行方式
- 单项：`go test ./test/waterline/ -run TestPickOfflineBusyGoesPending -v`
- 模块级：`go test ./test/waterline/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestComputeAdjustDeadZone` | \|diff\| = 0/2/3（死区边界） | 死区内不动；到边界才补/压 3 |
| `TestComputeAdjustClampAndGuards` | 差 100~400、目标 0、maxStep ≤ 0、死区负数 | 一律限幅到 ±maxStep；maxStep ≤ 0 不动 |
| `TestPickOfflinePrefersIdle` | 1 忙 + 3 空闲，要 2 个 | 只挑空闲（按输入顺序），无 pending |
| `TestPickOfflineBusyGoesPending` | 1 忙 + 2 空闲，要 3 个 | 空闲进 now；抓鬼中的忙号进 pending（不硬断） |
| `TestPickOfflineNotEnoughAndOfflineSkipped` | 只有 1 空闲 + 1 忙，要 5 个；含已离线号 | 有几个给几个；离线号不参与；`n<=0` 返回空 |
| `TestPickOfflineWithoutIdlePreference` | preferIdle=false（随机补选路径） | 按输入顺序取，但忙号仍只进 pending |
| `TestPickOfflineSubmitAndErrorGoPending` | SUBMIT（交付中）/ ERROR（卡住）与空闲号混排 | 只有空闲号进 now；SUBMIT/ERROR 只进 pending（不硬压） |
| `TestBusyJudgement` | 空闲 / 抓鬼会话 / 战斗 / DIALOG / WAIT_TASK(有/无任务) / 游荡 / SUBMIT / ERROR / WAIT_GHOST(有无会话) | 只有真在干活或异常的算忙；等待段（WAIT_GHOST 无会话）不算忙 |
| `TestPickOnlineCandidatesFilters` | 池里有不可用/已移除/已在线/在忙/空账号 | 只回可用离线号，按账号升序、受 n 限幅 |
| `TestTickDisabledDoesNothing` | enabled=false 且差得很远 | 一次动作都不发 |
| `TestTickSourceSvrThenLocalFallback` | 读数新鲜 / 过期 600s / 完全没有 | svr+fresh / local+不新鲜（svr 值仍展示）/ local+age=-1 |
| `TestTickTopUpWhenBelowTarget` | 缺 97 人 | 每轮补 5 个（升序取）；在途未落地不重复补；落地后只补未安排的号 |
| `TestTickDeadZoneNoAction` | 差 2（< 死区 3） | 不动作，差值如实展示 |
| `TestTickOfflineIdleNowBusyPendingThenRelease` | 超目标 400，4 空闲 + 1 抓鬼 | 空闲立刻断（4 个，绝不含抓鬼号）；抓鬼号进待下线；下一轮它收工后被断；无号可断时安静 |
| `TestTickSurplusRevokesPendingWhenBackInDeadZone` | 先超目标（标待下线）→ 读数回到目标附近 | 撤销待下线、不再多断号 |
| `TestTickMinKeepFloor` | 目标 0、min_keep=2、本地 3 个号 | 只压 1 个（保住自己的号） |
| `TestTickActionFailureKeepsLoopAlive` | 通道断了（下发失败） | 记 last_err、不算动作；下一轮照常恢复 |
| `TestConfigDefaultsAndPersistence` | 首次 Load / 保存 / 重启 Load | 默认 enabled=false（目标 100/死区 3/单轮 5/间隔 60/空闲优先）；参数原子落盘并原样恢复 |
| `TestSetConfigRejectsOutOfRange` | target -1/10001、dead_zone -1、max_step 0/51、interval 9/3601、min_keep -1；边界合法值 | 越界全部拒绝（错误信息点名字段）且不改内存参数；边界值放行 |
| `TestLoadPartialFileKeepsDefaults` | 残文件只写了 `{"enabled":true}` | 缺字段用默认值补齐（**不会**把 target 读成 0 = 把号全断） |

## 已知限制
- 不测真实上下线（`robot_manage add/remove` 的实际下发由 `test/api` 覆盖）；本模块只验证"该不该发、发给谁"。
- 不测 `Keeper.Start` 的 ticker 循环本身（`interval_sec` 生效路径）；轮询逻辑薄，行为等价于按间隔调 `Tick`。
- 随机补选只验证"顺序被打乱后仍按序取用"的结构，不验证随机分布（注入固定随机源保证可复现）。
- 壳层的候选摊平（账号池 + 运行时状态合并）未覆盖：它复用 `accounts.Pool.List` 与 `state.Snapshot`，由
  `test/api`、`test/accounts` 间接覆盖。
