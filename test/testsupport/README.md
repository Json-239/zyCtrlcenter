# testsupport 测试基座说明

> 规范单一事实源：[docs/04-测试/测试规范.md](../../docs/04-测试/测试规范.md)；
> 基座能力详解：[docs/04-测试/测试基座.md](../../docs/04-测试/测试基座.md)。
> 本包是**共享基座**，新测试不得重复造轮子。

## 提供的能力

| 能力 | 函数 | 说明 |
|---|---|---|
| 夹具加载 | `LoadFixture(t, name)` / `FixtureFiles(t)` | 读 `test/fixtures/events/*.json`（含 provenance） |
| 事件深拷贝 | `CloneEvent(t, ev)` | 同一夹具在用例内多次使用时防串改 |
| 临时环境 | `NewTestStore` / `NewTestLogger` / `NewTestHandler` | 全部落 `t.TempDir()`，不碰真实 `data/` |
| 假机器人 | `ConnectFakeRobot` / `SendEvent` / `SendRaw` / `ReadCmd` | 真实 TCP 连接控制通道 |
| 等待/断言 | `Eventually` / `WaitEvent` / `StoreHasType` | 轮询等待、控制通道事件、落盘校验 |

## 设计约束

1. **先隔离后使用**：任何用例不得直接读写项目 `data/`、`logs/`、`config.local.json`。
2. **不连真实外部**：不连机器人 exe、不连真实机器人进程；控制通道用 `Port=0` 系统分配。
3. **断言结果证据**：优先断言状态迁移 / 落盘数据 / 广播内容，禁止只断言「调用过某函数」。
