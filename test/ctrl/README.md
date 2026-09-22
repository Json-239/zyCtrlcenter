# ctrl 测试说明

## 被测对象
- 模块：`internal/ctrl/server.go`（控制通道，TCP JSON-lines 单连接）
- 关键函数：`Start` / `SendCmd` / `Connected` / `SetZone`（当前区标记）
- 协议见 [docs/03-协议/控制通道协议.md](../../docs/03-协议/控制通道协议.md)

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `ctrl_server_test.go` | 事件入队、命令下发、单连接接管、异常行容错、无连接失败、**写超时按载荷放宽 + 写失败断连（大载荷）** |

## 前置条件
- 端口用 `Port=0`（系统分配），互不干扰；假机器人走**真实 TCP**（`testsupport.FakeRobot`）。
- 不连真实机器人进程。

## 运行方式
- 单项：`go test ./test/ctrl/ -run TestNewConnectionTakesOverOld -v`
- 模块级：`go test ./test/ctrl/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestRobotEventReachesQueue` | 机器人发 hello | 事件解析入队，字段完整 |
| `TestSendCmdDeliversJSONLine` | 中控下发 status | 机器人端收到 `{"cmd":"status"}` |
| `TestSendCmdWithoutConnectionFails` | 无连接下发 | 返回 false（面板提示原因的依据） |
| `TestNewConnectionTakesOverOld` | robot 重连 | 新连接接管、旧连接被关闭、命令走新连接 |
| `TestGarbledLineIgnoredNextLineParsed` | 非法行/空行 | 忽略且不影响后续行 |
| `TestWriteTimeoutScalesWithPayload` | 写超时计算 | 5s 基础 + 每 256KB 1s，上限 60s（抓鬼 2MB 载荷 → 13s） |
| `TestSendCmdWriteTimeoutDropsConnection` | 机器人不读 + 收缓冲压小 + 反复下发 4MB 载荷 | `SendCmd` 返回 false；**连接被断开**（`Connected()=false`），后续下发直接失败——避免半行与下一条命令粘连成非法行 |

## 已知限制
- 未覆盖单行超过 64KB 的**上行**截断行为（`MaxLineBytes` 只约束机器人→中控；实现已限长，用例待补）。
- 未覆盖 `SetZone` 的并发读写（当前由 API 在配置变更时调用，事件读取走同一把锁）。
