# test/livecount：服务端在线数直连数据源（中控直接 HTTP 拉 /gm/online）

## 被测对象
- 模块：`internal/services/livecount/livecount.go`
- 关键函数：
  - 纯函数（可直接单测）：`ParseOnlineResp`（响应解析）、`Config.Normalize` / `Ready` / `StaleSec` / `Endpoint`
  - Provider：`Fetch`（单次拉取）、`Start`（周期轮询，默认关闭）、`Latest`、`Snapshot`
- 壳层接线（在 `test/api` 覆盖）：`internal/api/livecount.go` 的 `svrOnlineReading`（优先级
  provider → @online 回执 → local）与 `/api/status.svr_online` / `.livecount`

## 背景（参考实现依据）
- 只读参考：`game_admin_web`（PHP/FastAdmin）`origin/hqm` 分支：
  - `app/admin/library/GMApi.php:18/136`：`GET {GAME_SERVER_HOST}/gm/online?serverId=...` → `{online_count}`
  - `api-docs.json`：`/gm/online`（在线玩家数量）、`/gm/robotOnline`（机器人数）、`/gm/onlineList`
  - 调用链无鉴权头；实测 `http://192.168.0.201:8080/gm/online` 裸 GET 可用（见
    `docs/04-测试/分析-20260922-参考hqm实现在线人数.md`）
- 与既有 `@online` 口径一致：都是"全服在线角色数（含真实玩家）"；provider 只是换了一条**不需要
  DEPLOY 授权、不占用机器人**的通道。

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `livecount_test.go` | 响应解析 8 例（标准/0 值/data 包装/字符串数字/success=false/缺字段/非法体/robot 字段）；配置归一化与端点组装；Provider 拉取（成功/HTTP 错误/坏 JSON/无端点/失败后恢复清错）、Start（关闭零请求/起步立即拉/周期再拉/超时） |

## 前置条件
- 全部用 **httptest 假游戏服**（`/gm/online` 返回可控 JSON、记录收到的请求路径/query/头）。
  **不碰真实游戏服**、不起进程、不落盘。
- 默认口径取自实测与参考实现：`{"online_count":N,"success":true,"serverId":1000}`；
  配置默认 `enabled=false`、`server_id=1000`、`interval=60s`、`timeout=3s`。

## 运行方式
- 单项：`go test ./test/livecount/ -run TestParseOnlineRespStandard -v`
- 模块级：`go test ./test/livecount/ -v`
- 短模式（跳过 5s 周期用例）：`go test ./test/livecount/ -short`
- **真实探针**（默认跳过，不联网；对测试服 201 已实测通过）：
  `LIVECOUNT_PROBE_URL=http://192.168.0.201:8080 go test ./test/livecount/ -run TestProbeRealServer -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestParseOnlineRespStandard` | 实测标准响应 | count=32；无 robot 字段时 hasRobot=false |
| `TestParseOnlineRespZeroIsValid` | `online_count=0` | 合法读数（服里没人≠没有字段） |
| `TestParseOnlineRespDataWrapped` | PHP 层二次包装 `{data:{...}}` | 从 data 里取值（含 robot） |
| `TestParseOnlineRespStringNumber` | 字段值是字符串 `"45"` | 解析成功 |
| `TestParseOnlineRespSuccessFalse` | `success=false` + message | 报错且带服务端 message |
| `TestParseOnlineRespMissingField` | 只有 success / 其它字段 / 数组 | 一律报错（宁缺勿把 0 当值） |
| `TestParseOnlineRespBadBody` | 空/空白/HTML/null | 一律报错 |
| `TestParseOnlineRespRobotCount` | 只有 robot_online_count（那是 /gm/robotOnline） / 两字段合并 | 前者报错；后者两个数都取到 |
| `TestConfigNormalize` | serverID 空、interval 0/2/99999、timeout 0/999 | 回默认 / 夹到上限 |
| `TestConfigReadyAndStaleSec` | Ready 三态；StaleSec 默认 180 / 小间隔保底 90 | 与文档口径一致 |
| `TestConfigEndpoint` | 带/不带 serverID、base 尾部斜杠、空 base | 组装正确（含空 base 返回空串） |
| `TestProviderFetchOK` | 拉取成功 | count/ts/ok；请求路径 `/gm/online`、`serverId` query、`X-GM-Token` 头；Snapshot 全字段 |
| `TestProviderFetchHTTPError` | HTTP 500 | 报错含状态码；不留读数；`last_err`/`fails` 记账 |
| `TestProviderFetchBadJSON` | 非 JSON 响应 | 报错 |
| `TestProviderFetchNoEndpoint` | 未配 URL | 报错（说明端点缺失） |
| `TestProviderFetchFailureThenSuccessClearsErr` | 失败→恢复 | `last_err` 清空；tries/fails=2/1；读数=7 |
| `TestProviderStartDisabledMakesNoRequest` | enabled=false（含配了 URL） | Start 一个请求都不发 |
| `TestProviderStartFetchesImmediately` | 启用后 Start | 起步立刻拉一次；ctx 取消即退出 |
| `TestProviderStartTicksOverTime` | 间隔=5s（下限） | ≥2 次拉取（慢用例，-short 跳过） |
| `TestProviderFetchUsesTimeout` | 服务端慢响应 1.5s / 超时 1s | ~1s 超时报错、不留读数 |
| `TestProbeRealServer` | **真实探针**（默认 Skip；设 `LIVECOUNT_PROBE_URL` 才跑） | 对真实测试服拉一次 `/gm/online`，拿到 count/ts（2026-09-22 对 201 实测 count=3 ok=true） |

## 已知限制
- 不测"真实游戏服"可达性（网络/环境相关，见分析文档「需要用户配合」一节；实测命令已给）。
- 不测 `/gm/robotOnline` 的独立轮询（当前只做 `/gm/online`；robot 字段顺带解析，供将来"净真人"口径使用）。
- 不测 401/鉴权分支（该接口实测无鉴权；`X-GM-Token` 仅作将来兼容的预留头，已测其透传）。
