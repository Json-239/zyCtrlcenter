# test/intent：任务链路意图

覆盖 `internal/services/intent`（P0：判据 + 单链互斥 + 快照/计数）。

| 用例 | 输入 | 断言 |
|---|---|---|
| `TestDecideNewbieForLowLevel` | 20 级、新手链未完成 | 判新手链优先，带默认链 id 与理由 |
| `TestDecideGhostAtThreshold` | 31/32/45/90 级 | 判抓鬼；**边界 30 仍是新手链** |
| `TestDecideGhostWhenChainDone` | 20 级但 `chain_done` | 转抓鬼（别重复跑新手链） |
| `TestDecideUnknownLevelIsPending` | level 0 / -1 | **待定**（`known=false`）：不瞎判，等上线报等级 |
| `TestDeciderThresholdIsConfigurable` | 阈值 45 + 自定义链 id | 阈值/链 id 可配（避免与机器人端配置漂移） |
| `TestPlanSingleKindPerAccount` | 同一账号先新手链、再升到 45 级 | 切换返回 `prev`（要先停旧链）；`Conflict` 判"想跑别的链"；重复登记幂等（不刷 `since`） |
| `TestPlanRejectsEmptyAccountAndUnknownLevel` | 空账号 / 未知等级 | 明确报错；未知等级**不覆盖**已有意图 |
| `TestPlanSnapshotCountsAndRemove` | 3 个账号 | 快照按账号排序、`Counts` 分 kind、`Remove` 返回是否删到 |

口径与参考实现一致：`start_chain_done_level` / `NEWBIE_MAX_LEVEL` 默认 **31**。
接口侧（`GET /api/intents`）与事件驱动登记见 `test/api/intent*` 与 `docs/02-架构/任务链路模块化设计.md`。
