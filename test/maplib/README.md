# maplib 测试说明

## 被测对象
- 模块：`internal/maplib/maplib.go`
- 关键函数：`Load` / `Name` / `NameStr` / `All` / `Count` / `GridPos` / `GridPosWith`
- 口径说明：[docs/05-运维/坐标与地图.md](../../docs/05-运维/坐标与地图.md)

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `maplib_test.go` | 加载与查询、文件缺失/损坏容错、热更新、客户端格子坐标换算 |

## 前置条件
- 夹具：`test/fixtures/maps/maps.sample.json`（**真实数据的裁剪子集**，来自游戏 `config/map.csv`，带 provenance）。
- 全部落 `t.TempDir()`；不联网、不写项目 `data/`。

## 运行方式
- 单项：`go test ./test/maplib/ -run TestGridPosClientCoordinate -v`
- 模块级：`go test ./test/maplib/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestLoadAndLookup` | 加载裁剪后的真实表 | 条目数正确（跳过 `_comment`）、`11=长安东市集`/`24=幽冥界`；未知 id 返回空串；`NameStr` 支持字符串键 |
| `TestMissingFileIsEmptyNotError` | 表文件不存在 | 视为空表（不报错），查询返回空串 |
| `TestBrokenFileKeepsOldData` | 表文件损坏 | `Load` 报错，但**上次数据仍可用**（不清空） |
| `TestHotReloadOnFileChange` | 运行中改表文件 | 查询时自动热更新（无需重启） |
| `TestGridPosClientCoordinate` | 像素→格子 | `2108,809 → 131,50`（向下取整）；`GridCell=16`；可自定义格尺寸；非法格尺寸回退 16 |

## 已知限制
- 热更新按文件 `mtime+size` 判断（秒级时间戳文件系统上，同一秒内的改动可能延后一次检测）。
- 只做「id → 名称」与坐标换算，不解析 `map.csv` 的其它列（网格/缩略图尺寸等）。
