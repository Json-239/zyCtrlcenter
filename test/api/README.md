# api 测试说明

## 被测对象
- 模块：`internal/api/*`（HTTP 路由 + 可选鉴权 + CORS + WS 广播 + 协议映射）
- 路由表单一事实源：[docs/03-协议/HTTP-API.md](../../docs/03-协议/HTTP-API.md)

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `api_test.go` | 入口/状态/链文件驱动/下发内容/机器人管理/日志/协议映射/鉴权/404 |
| `start_auto_test.go` / `start_ghost_test.go` | 按意图自动分配 / **抓鬼带导航载荷、缺数据硬失败、拒绝导航数据、条件不匹配 warnings** |
| `restore_test.go` | 补发接口（手动触发、跳过在跑、**合并下发**） |
| `modules_test.go` | **模块地图接口**（注册表口径 + 最近一次测试报告） |
| `chainplan_test.go` / `intents_test.go` / `create_test.go` / `verify_test.go` / `verify_job_test.go` | 模块视图 / 意图表 / 建号 / 验证 |
| `create_throttle_test.go` | **注册自适应限速**（112 降速 / 连续成功回升 / 抖动 / 请求只能更保守）+ 建号响应 `throttle` 统计 |
| `livecount_test.go` | **服务端在线数来源优先级**（直连 provider `svr_provider` → @online 回执 `svr` → `local`）+ `/api/status.livecount` 诊断摘要（httptest 假游戏服） |

## 前置条件
- `httptest.Server` + **真实启动的测试控制通道**（`Port=0`，`testsupport.NewTestChannel`）+ 假机器人（`testsupport.FakeRobot`）。
- 抓鬼类用例装**两份**链数据夹具：`testsupport.InstallGhostNav`（基座 `newbie_full.mini.json` + 抓鬼专属 `zhongkui_nav.mini.json`）——中控按参考实现口径组装载荷，缺基座会硬失败。
- 区注册表用 `testsupport.NewTestZones`（单服单区；换区用例在用例内再加）。
- 数据目录/链目录/部署目录用 `t.TempDir()`；进程管理器指向临时目录假路径（不启动真实进程）。
- 「应用到机器人」用例会在临时部署目录里造一份 `script/config.py`（内容取自夹具）。

## 运行方式
- 单项：`go test ./test/api/ -run TestStartDeliversChainFromFile -v`
- 模块级：`go test ./test/api/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestIndexReturnsHint` | GET / | 中控提示 + ws + ports |
| `TestStatusIncludesZonesAndCurrent` | GET /api/status | 机器人行带区归属；候选区列表（含 `current`）；`ctrl_addr`/`ctrl_zone` 透出 |
| `TestStartWithoutRobotExplainsReason` | 无通道启动 | `ok=false` 且 msg 含「通道未连接」、`chain_source=none` |
| `TestStartDeliversChainFromFile` | 链文件存在 | 机器人**实际收到** chain（含未知字段、`grid_cell` 补齐）、`chain_source=file` |
| `TestStartPassesThroughInlineChain` | 请求体带 chain | 原样透传、`chain_source=request`、始终带 chain_id |
| `TestChainsEndpointFileDriven` | 空目录 / 正常文件 / `_template` / 缺失详情 | 列表按 id 升序且跳过模板；详情 `chain_found` 正确 |
| `TestMapsEndpoint` | GET /api/maps | 透出 `count`/`grid_cell=16`/地图表；真实数据抽查 `11=长安东市集`；`status.maps_count` 一致 |
| `TestAccountsListMergesPoolAndLive` | GET /api/accounts | 池（真实夹具 3 号）+ 运行时状态合并；**不返回明文密码**；按可用过滤；统计正确 |
| `TestAccountsAddAndRemove` | 加号 → 批量上线 → 下线 → 删号 | 服务端从池取密码下发 `[账号,密码]`（机器人实收）；下线本地标记移除 |
| `TestRobotsBatchOnlineAutoPick` | 批量上线（自动选号） | 只选该区可用号、跳过已在线、按 chunk 分批下发 |
| `TestRobotsBatchSelectionTrimsAndDedups` | 面板勾选/粘贴来的账号集合 | 去空白、丢空串、保序去重（重复账号只下发一次） |
| `TestMapGridEndpoint` | GET /api/map/grid | 用构造的 blockfile 校验 `w/h/rows`；未知 mapid 返回 `ok=false` 但 HTTP 200 |
| `TestRobotsManageAddDeliversPairs` | add 两种账号形式 | 机器人收到 `[[账号,密码]]`（缺省用默认密码）；remove 本地标记移除 |
| `TestStopAndResetDeliverAccounts` | 停链/重置 | 机器人收到 `stop`/`reset`，accounts 透传 |
| `TestLogsEndpointAndClear` | 日志读 + 清空 | 清空后只剩审计日志 |
| `TestProtocolsEndpointSections` | 协议映射 | 3 组且行非空；透出当前区 |
| `TestConfigGetAndSwitch` | GET /api/config + switch | 服/区列表与 `zones_file`；默认编码 UTF-8；切换后 `current`、`/api/status` 与控制通道事件标记同步；不存在的区失败但要 HTTP 200 |
| `TestConfigZoneUpsertAndDelete` | 区增删 | 新增生效、非法端口报错、删除后列表更新 |
| `TestConfigApplyWritesRobotConfig` | 应用到机器人 | 真实写 `script/config.py`：三键改写 + 其它保留 + 生成备份；再应用 → 不写盘 |
| `TestConfigApplyOtherZoneSwitchesCurrent` | 对指定区应用 | 会顺便切换当前区，config.py 写入目标区端口 |
| `TestRobotRestartFailsWhenExeMissing` | 重启进程（exe 缺失） | `ok=false` + 原因含「不存在」 |
| `TestOptionalAuthToken` | token 开启 | 无 token 401；`X-API-Token` 与 `Bearer` 放行；高危配置接口受保护、`config/switch` 不保护 |
| `TestReadOnlyEndpointsNotProtected` | 读接口 | 鉴权开启时仍可访问 |
| `TestNotFoundPath` | 未知路径 | 404 |

| `TestAccountsVerifyDefaultsToPoolBatchJob` | 不传 accounts → 按区从池选号批量 | 返回 job + 结果写回池 |
| `TestAccountsVerifyJobSkipsOnlineByDefault` | 在线号默认跳过 | 列出 skipped_online |
| `TestAccountsVerifyJobCancel` | 停止批量任务 | status=canceled |
| `TestAccountsVerifyZoneScopeAll` | 面板默认「本区全部」 | `scope=zone` + `limit=0`（不限）→ 该区有记录的 3 个全进（含已验过的），无记录的不进 |
| `TestIntentsEndpointDecideFlow` | 意图表接口 | 20 级上线→新手链；45 级→抓鬼（仍只一条）；`chain_done`→抓鬼；下线**保留**意图（供 P1 恢复） |
| `TestIntentsPendingWhenLevelUnknown` | 等级未知 | 不登记意图、不瞎判，接口正常返回 |
| `TestIntentsDecidedFromStatusHeartbeat` | 重连（只有心跳） | 心跳里的等级/链完成也判意图（source=status），否则中控重启后意图表为空 |
| `TestAccountsCreateRandomPasswordThenLoginWithStoredPassword` | 建号 → 随机密码入库 → 验证用库里密码 | 密码写进该区；登录帧 md5 与库中密码一致 |
| `TestAccountsCreateFailureDoesNotStorePassword` | 注册失败（已存在） | 不写密码、原因透出 |
| `TestAccountsCreateValidation` | 空账号 / 超 20 个 | 明确拒绝 |
| `TestAccountsCreateReusesExistingAccount` | 已存在且库密码正确 | **不发注册**（104 计数 0）；标可用 + 角色/等级；不改密码 |
| `TestAccountsCreateDetectsPasswordMismatch` | 已存在但库密码不符(200) | 不注册；标"存在但不可用"并说明怎么处理 |
| `TestAccountsCreateByNumberSpec` | 按编号（前缀+起始+数量+后缀） | 生成名字正确、每个号随机密码都写回库 |
| `TestAccountsCreateInBatchesWithConcurrency` | 每批 2 / 并发 2 | 6 个都建成且结果与输入同序 |
| `TestAccountsCreateAutoStartFromPool` | 自动接续 | 从池内同前缀最大序号+1 开始 |
| `TestAccountsCreateReplacesTooShortStoredPassword` | 库里老密码 3 位（脏数据） | 改用新的随机密码注册并写回库 |
| `TestStartAutoGhostCarriesNavPayload` | 抓鬼意图（45 级）启动 | 机器人实收 `ghost_start{chain_id:zhongkui_nav, chain{ghost_maps/ghost_map_pos/map_grids/dijkstra 非空}, role:solo}`；只发一条 |
| `TestStartGhostMissingNavFailsLoudly` | 链目录里没有导航数据 | `ok:false` + msg 明说；**一条命令都不发**（新手链那组也不发，避免半成功） |
| `TestStartRejectsNavOnlyChain` | 手选导航数据启动（非 auto） | 接口层拒绝：`ok:false, nav_only:true`，不下发命令 |
| `TestStartWarnsWhenConditionMismatch` | ≥31 的号手选 `newbie_full` | `ok:true` 但回带 `warnings[{account,msg}]`（提示该走抓鬼）并拼进 `msg`；命令照发 |
| `TestGhostNavAssemblesBaseAndGhostFields` | 抓鬼载荷组装（对齐参考实现 `ghost_nav_payload`） | 基座链提供 `npcs`（含钟馗 10146）/`map_grids`/`dijkstra`/`grid_cell`；专属文件提供 `ghost_maps`/`ghost_map_pos`/`maps`；载荷里**不混任务链字段** |
| `TestGhostNavRejectsIncompleteAddresses` | 地址不全（刷鬼图缺网格 / 缺落点） | 硬失败，错误点明缺哪张图 / 缺 `ghost_map_pos` |
| `TestIntentsRestoreMergesGhostAccountsIntoOneCommand` | 两个抓鬼号一起补发 | 机器人**只收 1 条** `ghost_start`（accounts 2 个 + 一份导航载荷）；`sent=2`/`commands=1` |
| `TestModulesEndpointRegistry` | GET /api/modules | 总数 12；动作层键集/顺序 == `chainplan.ModuleNames()`；每卡有职责/数据/参考实现/关联用例；无报告时 `report=null` |
| `TestModulesEndpointReadsLastReport` | 有 `data/test_report.json` | 报告内容原样透出 + `report_path` |
| `TestModulesEndpointBadReportDoesNotPanic` | 报告文件损坏 | 模块注册表照常返回，另给 `report_error` |
| `TestSvrOnlineSourcePriority` | 两路都无 → @online 回执 → provider 直连 → provider 失败回落 | `svr_online.source` 依次为 `local`（无 count）/`svr`/`svr_provider`/`svr`；`livecount` 摘要 ok/count 正确；水位保持器 `Deps.SvrOnline` 与 /api/status 同源 |
| `TestLiveCountSnapshotWithoutProvider` | 未装配 provider（默认关闭） | `/api/status.livecount` 与 `.waterline` 均为空对象（现状不回归） |

| `TestCreateThrottleConfigDefaults` | `config.Default()` 的 `CTRL_CREATE_*` | 自适应开、并发 8/≥2、批间隔 5s、抖动 ±2s |
| `TestCreateThrottleSlowsOn112AndCapsInterval` | 连续 112 | 并发 8→4→2（下限）、批间隔 5→10→20→40→60s（上限）；`total_112` 累加 |
| `TestCreateThrottleRecoversAfterSuccessStreak` | 降速后连续成功 | 满 10 次才 +1 / 间隔减半；并发回到上限止，间隔回到基准止 |
| `TestCreateThrottleNeutralErrorBreaksStreak` | 成功连击里夹一次中性错误 | 不回升（中断连击），也不降速 |
| `TestCreateThrottleAdaptiveOffOnlyCounts` | `Adaptive=false` | 节奏不变，但 112 仍计数（可观测） |
| `TestCreateThrottlePaceTakesConservativeSide` | 请求并发/间隔与限速器不同 | 并发取 `min`、间隔取 `max`（请求只能更保守）+ 抖动边界（±2s，不落负） |
| `TestAccountsCreateResponseCarriesThrottle` | 建号成功一次 | 响应 `throttle`：状态并发=上限 8、实际并发=请求 2、`total_ok=1`、无 112 |
| `TestAccountsCreateThrottleSlowsDownOn112` | 假服注册固定回 112 | 有效并发 8→4→2（连续两次）；结果带 `errid=112`，提示点明风控 |

## 已知限制
- WS 端到端推送未在此覆盖（握手/帧由 `test/wsutil` 覆盖，广播口径由 `test/event` 覆盖）。
- 未覆盖 `robot/restart` 的成功路径（会拉起真实进程，按铁律 3.2 不测真实副作用）。
- 单进程设计下不需要"多区并行"用例；跨区效果由 `test/zones`（注册表）+ `test/api`（应用/切换）覆盖。
