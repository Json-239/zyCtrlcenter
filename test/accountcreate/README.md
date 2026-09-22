# accountcreate 测试说明

## 被测对象
- 模块：`internal/accountcreate/accountcreate.go`
- 关键函数：`Register`（建号：握手 → 106 取验证码 → 104 注册 → 700 判结果）、`NewPassword`（随机密码）
- 协议：见 [游戏服探测协议](../../docs/03-协议/游戏服探测协议.md) §9

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `register_test.go` | 建号全链路 / 8 字段内容 / 失败原因 / 超时 / 拒连 / GBK / 随机密码 |

## 前置条件
- 假游戏服：`test/testsupport/gameserver.go`（服务端侧基座，按脚本回放并记录客户端实收帧）。
- 不联网、不写项目目录；端口用系统分配。

## 运行方式
- 单项：`go test ./test/accountcreate/ -run TestRegisterSendsEightFieldsAndReportsSuccess -v`
- 模块级：`go test ./test/accountcreate/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestRegisterSendsEightFieldsAndReportsSuccess` | 正常建号 | `OK=true` 且回带密码；104 实收 `[验证码, 账号, 密码, 密码, "", 密匙, "", ""]`；帧序 100→255→101→106→104（不发登录 102） |
| `TestRegisterFailureReasons` | 111 已存在 / 116 验证码错 / 112 风控 | 都不算 `Err`；说明含关键字；只有 112 `retryable=true` |
| `TestRegisterTimeoutWhenNoRegCode` | 服务端不回 702 | `Err` 含"超时"，不判成功 |
| `TestRegisterConnectionRefused` | 端口没人听 | `Err` 非空、不判成功 |
| `TestRegisterGBKFailsFast` | 编码=GBK | 快速失败且错误里点明 GBK（乱码会污染服务端库） |
| `TestNewPasswordRandomAndSafe` | 200 次生成 | 长度正确、字符集安全（无 `0/O/1/l/I`）、不重复；长度过短报错 |

> 「先验证再注册」的编排（已存在跳过注册 / 密码不符不注册）在 API 层：
> 见 `test/api::TestAccountsCreateReusesExistingAccount`、`TestAccountsCreateDetectsPasswordMismatch`。

## 已知限制
- 未覆盖 104 的"激活码/身份证/QQ"字段语义（当前留空，密匙写"姓名"位）。
- 未做批量建号的任务化（当前接口是同步批量，上限 20 个/次；`interval_ms` 默认 300ms 降风控）。
- 未覆盖服务端返回**未知 errid** 的真实样本（按"未收录错误"给通用说明）。
