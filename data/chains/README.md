# 链数据目录（data/chains）

> 本目录是**可选资产**：中控不内置任何链数据，链文件由使用方自行准备。
> 其余 `data/*`（运行历史/单 bot 日志）是运行时产物，不入库；本目录随项目入库。

## 约定

| 项 | 规则 |
|---|---|
| 文件名 | `<文件名>.json`；**文件名（去 .json）= 列表 id = 面板 `?id=` 查询值** |
| 链 ID | 文件内 `chain_id` 优先（没有才用文件名）；下发给机器人时以它为准 |
| 模板文件 | 以 `_` 开头的文件（如 `_example.json`）不参与 `/api/chains` 列表 |
| 内容 | 任意 JSON 对象；中控**原样透传**给机器人（未声明字段、字段形状都不改） |
| 缺失时 | 启动链只下发 `chain_id`（不带 `chain`），由机器人端决定怎么跑 |
| 损坏时 | 出现在列表里但带 `error` 字段，不影响其它链 |

## 字段（可选，中控解析用到的最小集合）

| 字段 | 用途 |
|---|---|
| `chain_id` / `name` | 列表与详情展示 |
| `start_task` / `end_task` | 面板展示起止任务号 |
| `task_order` | 数组，面板按「任务号/名称/接取 NPC/后续任务」渲染（形状任意，容错展示） |
| `task_hints` / `npcs` / `maps` / `map_grids` / `dijkstra` | 面板统计与下发给机器人的数据 |

> 其余字段（如 `ghost_map_pos`、`item_meta`、节点上的 `catcher_name` 等）不会被中控丢弃，
> 会原样出现在下发的 `start_chain.chain` 里（实现见 `internal/chainlib/passthrough.go`）。

## 从其它系统导出链数据

如果已有可生成链数据的系统（例如 Python 版 ctrlcenter 的 `chainlib.build_chain(...)`），
导出后直接放进本目录即可：

```powershell
# 示例：在源系统目录下执行，导出到本目录
& "<python.exe>" -c "import json,sys; sys.path.insert(0,'.'); import chainlib; json.dump(chainlib.build_chain('newbie_full'), open(r'<本项目>/data/chains/newbie_full.json','w',encoding='utf-8'), ensure_ascii=False)"
```

导出后建议跑一次链数据测试（原样透传 + 列表扫描）：

```powershell
go test ./test/chainlib/ -v
```
