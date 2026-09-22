# test/reghost：卡死自动重登恢复（钟馗对话卡死 → 下线 → 重登 → 延迟补发）

## 被测对象
- 模块：`internal/services/reghost/reghost.go`（状态机：纯决策 + 注入依赖）
- 触发：`internal/services/event` 收到 `error{code:GHOST_DIALOG_STUCK}` 时调 `Request`
- 口径对齐参考实现 `robot/ctrlcenter/services/event.py:_reghost_later`（重登上线后延迟 8s 补发；
  用户手动停止时清掉待恢复）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `reghost_test.go` | 正常路径（下线→重登→延迟补发→完成）、重复登记去重、取消即清、连续失败交人工 |

## 前置条件
- 假时钟 + 假动作（online/remove/add/launch 全是计数桩），不 sleep、不联网。

## 运行方式
- 单项：`go test ./test/reghost/ -run TestHappyPathOfflineReloginLaunch -v`
- 模块级：`go test ./test/reghost/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestHappyPathOfflineReloginLaunch` | 卡死 → 下线 → 离线 → 重登 → 上线 | 8 秒后补发一次并标记 done |
| `TestDuplicateRequestDeduped` | 同一号重复报错（error + ghost_offline） | 冷却期内只登记一次、只下一次线 |
| `TestCancelStopsRecovery` | 用户点「取消恢复」/停链 | 记录清空，之后不再上线/补发 |
| `TestFailureStopsAfterMaxAttempts` | 上线一直失败 | 到上限标 failed（交人工），不再无限重登 |

## 已知限制
- 不测真实机器人重登（`robot_manage remove/add` 由 `test/api` 与 `test/ctrl` 覆盖）。
- `done/failed` 状态保留 10 分钟后自动清理（面板确认窗口）。
