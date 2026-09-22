# test/chaindata：链数据客观闸门

**只用游戏服本机的客观数据**（`<游戏服>/config/map_file/blockfile`：0=可走 1=阻挡，格子=坐标/16）校验 `data/chains/*.json`，
不引用任何一方（含参考实现）的结论——链数据一变就跑，谁生成的都过同一道闸门。

| 用例 | 断言 |
|---|---|
| `TestTaskNpcPositionsAreWalkable` | 任务 NPC（catcher/thrower/前置战斗/商店）：所在图必须有网格、**坐标必须可走**、图必须在 `map_grids` 里 |
| `TestDynamicNpcBaselineUnchanged` | 没坐标的 NPC 必须与基线一致（动态 NPC 白名单）；增减都要人复核 |
| `TestGhostLandingPointsAreWalkable` | 抓鬼落点必须可走、刷鬼图必须在 `map_grids`；`knownBlockedLandings` 已清空（图9 的阻挡落点已由 `tools/fixlanding` 修好） |
| `TestDijkstraEndpointsAndPlan` | 每条路由两端图都能加载网格；真实链数据不能有组装错误 |
| `TestAllNpcPositionsAudit` | **npcs 全量抽查**（703 条坐标）：本链用到的图里阻挡坐标≤基线（非任务 NPC 允许站阻挡格，机器人靠近点击）；增加就报警 |
| `TestGhostNavAddressesAreComplete` | **抓鬼导航地址完备性**（中控侧组装口径）：基座链给钟馗坐标/网格/路由，抓鬼专属给刷鬼图/落点——每张刷鬼图都必须有网格与落点（缺了就是"原地不动/空点钟馗连点 15 次"） |

跳过条件：本机没有游戏服配置目录 → `t.Skip`（可用 `ZY_GAME_CONFIG` 指定），CI/别的机器不会误红。

相关工具：`go run ./tools/fixlanding`（按可走格重选抓鬼落点，`-write` 写回；dry-run 默认）。
本次用它修掉了参考实现落点算法选到的阻挡格（图9 `(481,604)` → `(787,780)`）。
