# test/chainplan：链路模块化组装

覆盖 `internal/chainplan`：把链数据（提示表）展开成**模块序列**并做结构校验。
中控不执行链（执行在机器人端），这里做的是"翻译 + 提前报坑"，同时作为**两边审查**的锚点（模块名与参考实现执行器一一对应）。

| 用例 | 断言 |
|---|---|
| `TestBuildExpandsModuleSequenceInOrder` | 模块顺序：前置战斗 `pathfind→move→fight→restore` → 接取 `pathfind→(cross_map)→move→talk` → `buy` → `wait`；购买必须在接取之后 |
| `TestModuleNamesMatchReferenceExecutors` | `shop_purchase/alloc_point/fight_npc` → `buy/alloc/fight`；`ModuleNames()` 含模块全集 |
| `TestValidationRejectsUnknownExecutor` | 未知 `executor` 报错并点名（fail loud，别静默跳过） |
| `TestValidationDanglingNext` | 区间内 `next` 悬空=**错误**；指向区间外=**提示**（下一段链） |
| `TestDanglingNextOnlyErrorsInsideMeaningfulRange` | 真实新手链 `start=7001001→end=5010105` 跨 XML 段（数值上 start>end）时 end 不能当区间上界：链外后继只提示；`next==end_task` 连提示都没有 |
| `TestValidationShopHintArgs` | 缺 `shop.npc`=错误；缺 `item_index`=提示；购买参数原样进 `args` |
| `TestNpcPositionsAcceptFlatAndNested` | `npcs` 兼容扁平 `[map,x,y]`（真实导出）与嵌套 `[[map,x,y],…]`（模板）；寻路带候选图、跨图带 `target_map` |
| `TestWarningWhenNpcHasNoPosition` | NPC 无坐标只提示（等动态 NPC 推送 90351） |
| `TestRealFixtureEmptyOrderIsStable` | 真实夹具（`task_order` 空）→ 只有提示 + `active_accept`；**重复 50 次组装结果逐字节一致** |
| `TestStepsAreWellFormed` | `order` 递增、`source ∈ {data,derived,policy}`、模块计数=步骤数、`restore` 必须标 policy |
| `TestActiveAcceptOnlyForWhitelistedChain` | 只有 `zhuaogui` 标主动接取（参考实现按链 id 特判） |

接口侧：`GET /api/chains?id=` 的 `plan` / `plan_error` 见 `test/api/chainplan_test.go`；
设计口径见 `docs/02-架构/任务链路模块化设计.md`。
