# test/process：机器人进程管理

## 被测对象
- 模块：`internal/services/process/process.go`（拉起/停止/重启机器人进程）、`orphans.go`（按部署目录清理残留）
- 关键行为：**拉起前清理「同部署目录的其它机器人进程」**，保证同目录只有一个（否则两个进程抢同一条控制通道）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `orphans_test.go` | `ParsePIDs`：命令行工具输出（CRLF/空行/空白/非数字）→ PID 列表 |

## 前置条件
- 纯函数测试：不启动真实进程、不联网（真实清理在 Windows 上按部署目录前缀匹配，见 `orphans.go` 注释）。

## 运行方式
- 单项：`go test ./test/process/ -run TestParsePIDs -v`
- 模块级：`go test ./test/process/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestParsePIDs` | CRLF 多行 / 空行 / 前后空白 / 空输入 / 非数字 / 非正数 | 只保留合法 PID，顺序不变；非法行忽略 |

## 已知限制
- 未覆盖真实 `killOrphansInDeployDir`（要起真实进程并调系统命令；靠线上验证：重启中控后确认同目录只有一个机器人）。
