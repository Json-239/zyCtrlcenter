# test/recover：恢复引擎（P1）

覆盖 `internal/services/restorer`：按**意图**补发命令，让账号一直跑在该跑的链上（中控重启/机器人重连/任务丢了之后）。

| 用例 | 断言 |
|---|---|
| `TestTickStartsChainForIdleAccount` | 意图=newbie + 在线 + 空闲 → 补发 `start_chain(newbie_full)` |
| `TestTickGhostUsesGhostStart` | 意图=ghost → 补发 `ghost_start` |
| `TestTickSkipsRunningDoneAndAdvancing` | `NAV/CLICK/DIALOG/FIGHT/SHOP/ALLOC/WAIT_NEXT/DONE/ERROR` 不打扰；`WAIT_TASK` 有任务号=链在推进也不打扰，**没有任务号才补**（参考实现补拉判据） |
| `TestTickSkipsWhenDisabledChannelDownOrOffline` | 总开关关着 / 通道没连 / 不在线 / 意图=idle → 都不发 |
| `TestTickCooldownRetryAndCircuitBreaker` | 冷却 300s → 重试 → 3 次上限后熔断 1800s，过期自动解除 |
| `TestTickResetsAttemptsOnceRunning` | 补发后真的跑起来 → 尝试计数归零（算恢复成功，别误熔断） |
| `TestTickCircuitsOnRepeatedErrors` | 同一错误重复 ≥3 → 直接熔断等人工 |
| `TestTickDeterministicOrder` | 多账号按账号排序（重复 20 次结果一致） |
| `TestGhostRestoreCarriesPayload` | 抓鬼补发按 `kind=ghost` 取载荷；载荷可取 → 正常产出补发决策 |
| `TestRestoreSkipsWhenPayloadMissing` | 载荷取不到（导航数据缺失/改坏）→ **不瞎发**：记失败并说明，连续 3 次熔断等人工 |
| `TestGroupActionsMergesByCommandAndChain` | 同命令 + 同链合并成一条下发（账号保持排序、去重）；不同链不合并 |

接口：`GET /api/intents`（带 `auto_restore` / `recover`）与 `POST /api/intents/restore`（手动补发）见 `test/api/restore_test.go`。
口径与参数出处见 `docs/02-架构/任务链路模块化设计.md` §1（参考实现 `intent_restore.py` / `idle_wander.repull_task_for`）。
