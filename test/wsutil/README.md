# wsutil 测试说明

## 被测对象
- 模块：`internal/wsutil/ws.go`（零依赖最小 WebSocket 服务端）
- 关键函数：`Upgrade` / `WriteText` / `ReadMessage`

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `ws_test.go` | 握手 Accept 计算、掩码帧解析、文本帧回显、非 WS 请求拒绝 |

## 前置条件
- `httptest.Server` + **手写原始 TCP 客户端**（不引第三方 WebSocket 库，验证与标准协议互通）。
- 不联网。

## 运行方式
- 单项：`go test ./test/wsutil/ -run TestUpgradeHandshakeAndEcho -v`
- 模块级：`go test ./test/wsutil/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestUpgradeHandshakeAndEcho` | 完整握手 + 掩码文本帧 | 101 + `Sec-WebSocket-Accept` 正确 + 回显帧无掩码 |
| `TestUpgradeRejectsNonWebsocket` | 普通 GET | 升级失败（不返回 101） |

## 已知限制
- 不支持分片聚合（客户端大消息）；中控只消费 ping/pong/close，业务消息不解析。
