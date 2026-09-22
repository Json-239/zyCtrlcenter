# chainlib 测试说明

## 被测对象
- 模块：`internal/chainlib/chainlib.go` + `internal/chainlib/passthrough.go`
- 关键函数：`Build` / `List` / `FilePath`；`Chain` 的未知字段与形状透传

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `chainlib_test.go` | 文件缺失/空 ID、加载与缺省补齐、未知字段与形状透传、目录扫描容错 |

## 前置条件
- 夹具：`test/fixtures/chains/zhuaogui_nav.sample.json`（真实导出裁剪，带 provenance）。
- 全部数据目录用 `t.TempDir()`；不联网、不写项目 `data/`。

## 运行方式
- 单项：`go test ./test/chainlib/ -run TestBuildPreservesUnknownFieldsAndShapes -v`
- 模块级：`go test ./test/chainlib/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestBuildMissingFileReturnsErrNotFound` | 链文件不存在 | 返回 `ErrNotFound`（调用方据此只下发 `chain_id`） |
| `TestBuildRejectsEmptyID` | `chain_id` 为空 | 报错 |
| `TestBuildLoadsAndNormalizes` | 只有 chain_id/start/end | `grid_cell=16`，空表为非 nil 空对象/空数组 |
| `TestBuildPreservesUnknownFieldsAndShapes` | 真实形状夹具（含 `ghost_map_pos`/`item_meta` + 扁平 `npcs` + 任意形状 `task_order`） | 未知字段与形状原样透传、内容一致 |
| `TestListScansDirectory` | 目录不存在/为空/含 `_template`/损坏文件/子目录 | 空目录返回空；跳过模板与子目录；损坏文件带 `error` 且不影响其它 |
| `TestFilePath` | 路径拼接 | `<chainDir>/<id>.json` |

## 已知限制
- 不做链数据语义校验（网格合法性、任务号区间等）——中控只搬运。
- 未覆盖超大文件（数十 MB）加载性能。
