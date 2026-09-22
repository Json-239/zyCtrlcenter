# zones 测试说明

## 被测对象
- 模块：`internal/services/zones/zones.go`（服/区注册表：单进程切换式）+ `internal/services/zones/robotcfg.go`（写机器人 config.py）
- 关键函数：`Load` / `Switch` / `UpsertServer` / `UpsertZone` / `RemoveZone` / `RemoveServer` / `Flat`；
  `ApplyRobotConfig`

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `zones_test.go` | 种子/二次加载、切换与持久化、增删改、校验、展平与编码继承 |
| `robotcfg_test.go` | 只改三个键、保留其它内容与注释、写前备份、无变化不写、缺键追加、参数校验 |

## 前置条件
- 全部落 `t.TempDir()`（注册表 JSON / 假 config.py 都自建）；不联网、不碰真实 `data/`。
- **编码口径**：默认 `UTF-8`（`zones.DefaultCoding`）；`GBK` 仅作为区级可选覆盖值（用例里用 GBK 验证"覆盖生效"）。
- config.py 夹具：`test/fixtures/robotcfg/config_py.sample.json`
  （构造，`real=false`，来源=参考项目运维手册 §4.1；编码值写成旧的 GBK，用于覆盖「旧编码 → UTF-8」改写场景）。

## 运行方式
- 单项：`go test ./test/zones/ -run TestApplyRobotConfigRewritesOnlyThreeKeys -v`
- 模块级：`go test ./test/zones/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestSeedOnFirstLoad` | 首次加载（文件不存在） | 写入种子（1 服 2 区，UTF-8）、展平正确、二次加载不重复播种 |
| `TestSwitchPersistsAndValidates` | 切换当前区 | 返回目标区、持久化（重载仍生效）、不存在返回 `ErrNotFound` |
| `TestUpsertZoneAndUpdate` | 新增/更新区 | 新 key 缺省=端口；同 key 为更新；区级编码可覆盖、未覆盖继承服级 |
| `TestRemoveZoneAndServerFallbackCurrent` | 删除区/服 | 删当前区回退剩余第一；删不存在静默；删服连带删区 |
| `TestNormalizeValidation` | 校验与规整 | host 空/端口非法/编码非法报错；去空白；服内重复区 key 去重 |
| `TestFlatInheritsServerCoding` | 展平 | 编码缺省继承服、区级可覆盖；`addr=host:port` |
| `TestApplyRobotConfigRewritesOnlyThreeKeys` | 写入 config.py | 只改 `ip/port/PROTOCOL_CODING`；注释与其它配置保留；生成备份 |
| `TestApplyRobotConfigNoChangeSkipsWrite` | 值已一致 | 不写盘、不备份、全部标记 `unchanged` |
| `TestApplyRobotConfigMissingKeyAppended` | 文件缺键 | 末尾追加（带标记），不破坏原内容 |
| `TestApplyRobotConfigSkipsExpressionValues` | 值是环境变量表达式（现场 config.py 形状） | **跳过不改**（`skipped=[ip,port]`）、表达式行一字不动、括号配对、字面量键仍可改写 |
| `TestApplyRobotConfigValidates` | 参数校验 | 文件不存在/host 空/端口非法/编码非法均报错 |

## 已知限制
- 未覆盖并发调用（注册表内部用互斥锁串行化，未做并发压力用例）。
- `ApplyRobotConfig` 按行匹配"键 = 字面量"；表达式值（`_os.environ.get(...)` 等）按设计**跳过**并提示，不做自动改写。
