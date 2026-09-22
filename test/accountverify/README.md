# accountverify 测试说明

## 被测对象
- 模块：`internal/accountverify/accountverify.go`
- 关键函数：`Probe`（单账号）、`VerifyAll`（批量，结果与输入同序）、`DefaultOptions` / `Options`
- 链路：`100 握手 → 200/255 → 101 版本 → 400 → 102 登录 → 300 验证返回 →（可选）1000/90132 查角色`
  （见 [游戏服探测协议](../../docs/03-协议/游戏服探测协议.md)）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `probe_test.go` | 正常链路 / 100-200-300 判定 / 选项 / 编码 / 超时 / 拒连 / ctx 取消 / 批量 |

## 前置条件
- 假游戏服：`test/testsupport/gameserver.go`（**服务端侧基座**，控制通道那侧已有 `FakeRobot` 客户端侧）；
  按脚本回放帧，并记录"客户端实际发了什么"，回什么完全由用例决定（返回 nil = 不回，用于测超时）。
- 不联网、不写项目目录；端口用系统分配。

## 运行方式
- 单项：`go test ./test/accountverify/ -run TestProbeUsableWhenLoginOKWithRole -v`
- 模块级：`go test ./test/accountverify/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestProbeUsableWhenLoginOKWithRole` | retcode=0 + 一个角色 | `exists`/`usable` 真；角色名/等级正确；**实收帧顺序 100→255→101→102→1000**、255 带 200、101 带版本号、102 是 `[账号, md5]`、1000 带 0 |
| `TestProbeUsableWhenLoginOKWithoutRole` | retcode=0 但无角色 | 仍可用；说明含「无角色」 |
| `TestProbeNoAccount` | retcode=100 | `exists/usable` 均假；说明含「不存在」；**不再发 1000** |
| `TestProbeBadPasswordIsNotUsable` | retcode=200 | `exists` 真、`usable` 假（密码错≠可用） |
| `TestProbeFreezedIsNotUsable` | retcode=300 | `exists` 真、`usable` 假 |
| `TestProbeSkipsRoleQueryWhenDisabled` | `query_role=false` | 不发 1000，仍可用 |
| `TestProbeGBKZoneFailsFastNotSilently` | 区编码=GBK | 快速失败且错误里点明 GBK（不等超时、不判可用） |
| `TestProbeTimeoutWhenNoAck` | 300 一直不来 | `Err` 含「超时」，等满超时才放弃，且不判可用 |
| `TestProbeConnectionRefused` | 端口没人听 | `Err` 非空、不判可用、`exists=false` |
| `TestProbeStopsOnContextCancel` | ctx 200ms 取消、Timeout=5s | 随 ctx 提前返回（<2s） |
| `TestVerifyAllKeepsInputOrderAndUsesOneConnPerAccount` | 3 个账号并发 | 结果与输入同序；每账号一条独立连接（3 条） |
| `TestVerifyAllEmptyAndBadConcurrency` | 空输入 / `concurrency=0` | 空输入返回空；0 按 1 处理（不 panic、不死锁） |
| `TestBuildRoleFrameMatchesReferenceFixture` | 用 `BuildRoleFrame` 反构角色帧 | 与参考实现生成的夹具字节一致 |

| `TestJobRunsAllAndCallsBackPerAccount` | 批量任务 3 个账号并发 2 | 进度 3/3、分类计数、结果与输入同序 |
| `TestJobOnResultCallbackPerAccount` | 逐条回调 + 结束回调 | 每个账号回调一次（供增量写回池） |
| `TestJobCancelDiscardsInFlightResults` | 跑到一半停止 | 状态 canceled、未完成不计入、**不回调** |
| `TestJobManagerLookup` | 按 ID / 空 ID / 未知 ID 取任务 | 分别返回任务 / 最近任务 / nil |

## 已知限制
- 不校验"密码是否正确"以外的账号属性（等级/装备等业务字段不在探测范围）。
- 未覆盖游戏服返回**未知 retcode** 的真实样本（当前按"存在但不可用 + 带原始码"处理）。
