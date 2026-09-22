# accounts 测试说明

## 被测对象
- 模块：`internal/services/accounts/accounts.go`（账号池）
- 关键函数：`Load` / `List` / `Stats` / `Pick` / `Add` / `Remove` / `SetZoneState` / `Password` / `Meta`
- 业务说明：[docs/05-运维/账号池.md](../../docs/05-运维/账号池.md)

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `accounts_test.go` | 加载（真实库裁剪）、过滤/分页、统计、选号、增删、区状态写入、**重新导入热更新**、密码回退 |

## 前置条件
- 夹具：`test/fixtures/accounts/accounts.sample.json`（**真实账号库裁剪**：3 个号 + 其在 `47.96.8.240:2300` 的 usable/verified 状态）。
- 全部落 `t.TempDir()`；不联网、不碰真实 `data/`。

## 运行方式
- 单项：`go test ./test/accounts/ -run TestPickForBatchOnline -v`
- 模块级：`go test ./test/accounts/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestLoadAndQueryFromRealFixture` | 加载真实裁剪池 | 3 个号、字段与区状态（usable/verified/msg）正确；Meta 有来源与数量 |
| `TestFilterAndStats` | 过滤/分页/统计 | 按区可用过滤（2 个）、按角色名关键词命中、limit/offset 生效；统计含 assigned/usable/chain_done |
| `TestPickForBatchOnline` | 选号 | 只选可用号；可排除已在线；limit 生效；不过滤可用性时全选 |
| `TestAddRemoveAndZoneState` | 增删与区状态 | 新增标记 added/existed；写入区状态后 `UsableIn` 为真；删除计数正确 |
| `TestHotReloadAfterReimport` | 外部重新导入（覆盖池文件） | 查询时自动热更新（数量与新号可见），**无需重启** |
| `TestPasswordFallback` | 取密码 | 池内有密码用它；不在池里回退默认密码 |

| `TestSelectForVerifyUnverifiedOnlyCountsKnownZoneRows` | 选号口径：陌生区 / 该区有记录未验证 | 只选"该区有记录"的号，陌生区靠 unknown |
| `TestZoneStateVerifiedAtPersists` | 验证时间落盘 | `verified_at` 读写一致 |
| `TestPasswordOnlyFromPool` | 密码只从库里取 | 不在池里 → 空 + ok=false（**没有统一密码**） |
| `TestPasswordForPrefersZoneThenAccount` | 区级密码优先、回退账号级 | 区级 `password` 优先且能落盘读回 |
| `TestCreateResultWrittenBackKeepsPasswordAndUnverified` | 建号回写 | 随机密码可取；该区状态为"未验证" |
| `TestPasswordSharedWithinSameServer` | 同服通用 | 同 host 的区共享密码；别的服不共享；精确区优先 |
| `TestNoSaveBatchSurvivesHotReload` | 批量写回 vs 热重载 | 未落盘的内存改动不能被重载冲掉 |
| `TestBuildNames*`（5 个） | 按编号生成账号名 | 前缀+补零+后缀、自动接续、参数校验、去重不撞库 |
| `TestSelectForVerifyZoneScope` | 「本区全部」范围 | 该区有记录的全部（含已验过）、无记录不算；limit 0/负 = 不限；计数给 `zone` |
| `TestSetZoneStateNoSaveThenSave` | 批量写回：先写内存后统一落盘 | 内存即时生效、Save 后与内存一致 |

## 已知限制
- 未覆盖并发写（池内用互斥锁串行化，未做压测）。
- 「批量注册/验证」尚未实现（需移植游戏协议，见 [账号池文档](../../docs/05-运维/账号池.md) §6），因此池里的 `usable/verified` 目前来自**导入**而非中控自己验证。
