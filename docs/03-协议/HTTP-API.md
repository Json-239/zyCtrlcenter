# HTTP API（中控 ↔ 前端）

- 基础地址：`http://127.0.0.1:28082`（`--web-port` / `CTRL_WEB_PORT` 可改）
- CORS：`Access-Control-Allow-Origin: *`（面板可部署在任意端口/机器）
- 前端：`web/`（Vite dev server `:5273`，proxy `/api`、`/ws` → 28082）
- `GET /` 只返回一行 JSON 提示；未知路径返回 **404 JSON**（不是 HTML）
- 实现：`internal/api/`

## 1. 路由总表（36 API + 1 WS + 入口）

| 方法 | 路径 | 请求体 | 返回概要 |
|---|---|---|---|
| GET | `/` | — | `{name, version, frontend, api, ws, ports{web,ctrl}}` |
| GET | `/api/status` | — | `{ok, version, robots[](含 zone/pos/pos_grid), counts{total,online,handshake}, zone_counts, zones[](候选区，含 current), current, current_keys, removed[], robot_connected, ctrl_addr, ctrl_zone, robot_running, robot_exe, robot_exe_exists, server, task_failed{count,items}, chains, maps_count, grid_cell, ws_clients, logs_file, data_dir, chain_dir, zones_file, deploy_dir}` |
| GET | `/api/config` | — | `{ok, servers[], zones[](含 current), current, current_keys, zones_file, updated_at, defaults{ctrl_port,deploy_dir,config_path,codings,default_coding}, robot_config_hint}` |
| POST | `/api/config/switch` | `{server?, zone \| key}` | `{ok, zone, current, msg}` 切换当前区（低风险，**不鉴权**） |
| POST | `/api/config/servers` | `{server:{key,name,host,coding,note,zones[]}}` | `{ok, server, added, msg}`（**可选鉴权**） |
| POST | `/api/config/zones` | `{server, zone:{key,name,port,coding,note}}` | `{ok, zone, added, msg}`（`key` 留空=用端口；`coding` 留空=继承服）（**可选鉴权**） |
| POST | `/api/config/servers/delete` | `{server}` | `{ok, removed, msg}`（**可选鉴权**） |
| POST | `/api/config/zones/delete` | `{server, zone}` | `{ok, removed, msg}`（**可选鉴权**） |
| POST | `/api/config/apply` | `{zone?, restart_robot?}` | `{ok, zone, addr, coding, config_path, result{applied,changed,backup,missing}, restarted, msg}`；`zone` 省略=当前区，指定别的区会顺便切当前区（**可选鉴权**） |
| GET | `/api/chains` | `?id=<文件名>` | `{ok, chain_dir, chains[{id(文件名),chain_id(文件内声明),name,start_task,end_task,task_count,file,error?}]}`；带 `id` 时附 `{chain, chain_found, msg?}` + **`plan`（模块化组装视图：`tasks[].steps[{order,module,action,args,source,note}]`、`modules` 用量、`warnings`、`active_accept`）** 与 `plan_error`（结构问题：悬空 next / 未知执行器 / 缺 shop.npc） |
| GET | `/api/maps` | — | `{ok, count, grid_cell, source, maps{mapid: 名称}}` 地图名表 + **客户端坐标口径** |
| GET | `/api/tasknames` | `?id=2019508` | `{ok, count, available, dir, names:{"2019508":"捉鬼",…}, id?, name?}` **任务号 → 任务名（只读）**：名字来自游戏配置 `<GameConfigDir>/task/*.xml` 的 `task_entry.episode_name`（回退 `name`）；懒加载 + 缓存；目录不可用时返回空表（面板回退显示编号），不报错 |
| GET | `/api/modules` | — | `{ok, layers[3], count:12, modules[{key,name,layer,desc,data,py,tests[]}], report:{…最近一次 tools/test_report 的产物…}, report_path, report_error?}` **模块地图（只读）**：动作层 9 个模块的键集/顺序 == `chainplan.ModuleNames()`（有单测钉住）+ 链数据 2 + 编排 1；`data/test_report.json` 缺失时 `report=null`（面板显示"—"，不是错误），文件坏了回 `report_error` |
| GET | `/api/map/grid` | `?mapid=11` | `{ok, mapid, name, grid{w,h,rows}, grid_cell, available, config_dir}` 网格（地图可视化底图） |
| GET | `/api/accounts` | `?zone=&usable=1&online=1&keyword=&limit=&offset=` | `{ok, count, pool, stats, zone, meta, accounts[{name,level,role_name,usable,verified,verify_msg,online,state,runtime_zone,has_password,…}]}`（**不下发密码**） |
| GET | `/api/accounts/verify/job` | `?id=` | `{ok, job{id,status,total,done,usable,unusable,not_exists,error,results…}}`（`id` 空=最近任务） |
| POST | `/api/accounts/verify/cancel` | `{id?}` | `{ok, job, msg}` 停止批量验证（**可选鉴权**） |
| GET | `/api/accounts/stats` | `?zone=` | `{ok, stats{total,assigned,usable,chain_done}, pending{unverified,unusable,unknown,all}, online_total, robot_total, meta}`（`pending` 供面板显示"本次将验 N 个"） |
| POST | `/api/accounts/add` | `{accounts\|names, password?, zone?, note?}` | `{ok, added[], existed[], pool, msg}`（**可选鉴权**） |
| POST | `/api/accounts/remove` | `{accounts}` | `{ok, removed, msg}` 从池删号（**可选鉴权**） |
| POST | `/api/accounts/create` | 指定账号 `{accounts}`；或**按编号** `{prefix, start?, count, suffix?, pad?, auto_start?}`；另有 `{zone?, password?, password_len?, batch_size?, concurrency?, interval_ms?, batch_interval_ms?, agent_key?, timeout_sec?, persist?}` | `{ok, mode:"create", results[{account,status,ok,ret_code,exists,usable,password?,err_id,retryable,role_name,level,msg}], created, existing, password_mismatch, failed, first, last, names_count, batch_size, concurrency, pool_updated, msg}` **建号/校验**：先验证再注册（status=`created/existing/password_mismatch/failed`）（**可选鉴权**） |
| POST | `/api/accounts/verify` | 带 `accounts`=同步（≤200）；不带=**批量任务**（`zone?, scope?=zone\|unverified\|unusable\|unknown\|all, limit?=0 表示不限, skip_online?`；面板默认 `scope=zone` 本区全部） | `{ok, zone, game_addr, coding, version, summary{total,exists,usable,unusable,not_exists,error}, results…, pool_updated, mode}` 账号验证（**可选鉴权**） |
| POST | `/api/robots/batch` | `{action:"online"\|"offline", accounts?, zone?, limit?, chunk?, interval_ms?, only_usable?}` | `{ok, requested, sent, chunks, accounts[], skipped_online[], skipped_no_password[], msg}` 批量上下线（面板点选/勾选来的账号会**自动去空白/空串/保序去重**，已在线的跳过）（**可选鉴权**） |
| POST | `/api/intents/restore` | `{account?}` | `{ok, sent, commands, skipped, actions[{account,command,chain_id,reason,sent,msg?}], msg}` **立即按意图补发一次**（`start_chain`/`ghost_start`/`share_daily_start`）：冷却/重试上限/熔断仍生效，跳过在跑/跑完/离线的号；**同命令 + 同链的号合并成一条下发**（抓鬼载荷 2MB 级，逐号一条 = N×2MB），所以 `sent`=覆盖账号数、`commands`=实际命令数；抓鬼补发同样带 `chain` + `role=solo` + `daily_limit`；分享日常补发带 `share_key` + `daily_limit` + `done?`；载荷取不到（基座/专属文件缺失、地址不全）→ 该批不下发并在 `actions[].msg` 说明（**可选鉴权**） |
| GET | `/api/intents` | — | `{ok, count, counts{newbie|zhuaogui|ghost|shenbu|idle}, intents[{account,kind,chain_id,zone,source,reason,since}], newbie_max_level, newbie_chain_id, zhuaogui_chain_id}` **账号意图表（只读）**：等级<31 → 新手链优先，≥31 或已完成 → 抓鬼，等级未知不登记；分享日常开关开启时 ≥40 且心跳报"今日未满" → 大唐神捕（`kind=shenbu`）；一个账号同一时刻只有一条链（P0；P1 按它补发命令） |
| GET | `/api/logs` | `?n=200` | `{logs:[…]}` 当天运行历史尾部（`n` 上限 5000） |
| POST | `/api/logs/clear` | — | `{ok, removed_runs, removed_bot, msg}`（**可选鉴权**） |
| GET | `/api/protocols` | — | `{ok, updated, current{version,coding,ctrl_addr,web_port,robot_exe,connected}, sections[3]}` 协议映射（只读） |
| POST | `/api/start` | `{chain_id?, chain?, accounts?, auto?}` | **`auto=true` = 按账号意图自动分配**：新手链/捉鬼链意图 → `start_chain(带链数据)`、抓鬼意图 → `ghost_start`（**组装出的导航数据 `chain` + `role:"solo"` + `daily_limit` + `done?`**；基座链或抓鬼专属文件缺失、刷鬼图缺网格/落点 → `ok:false` 且**一条命令都不发**）、大唐神捕意图（分享日常开关开启时）→ `share_daily_start`（`share_key` + 组装链载荷 + `daily_limit` + `done?`；声明文件缺失/缺 task_order 同样硬失败一条不发）、无意图 → 回落 `chain_id`、空闲 → 不发；返回 `{mode:"auto", groups{start_chain[],ghost_start[],share_daily_start[]}, assignments[{account,command,chain_id,reason}], sent, msg}`（大屏「启动」默认走它）；给 `chain_id` 但不给 auto 时：选中的是**导航数据（没有任务节点）→ 直接拒绝**（`ok:false, nav_only:true`），否则按号回带 `warnings[{account,command,chain_id,msg}]`（如"该号已判抓鬼/大唐神捕，建议用自动分配"） | `{ok, msg, chain_id, chain_source(request\|file\|none), warnings[]}` |
| POST | `/api/stop` | `{accounts?}` | `{ok, msg}` |
| POST | `/api/reset` | `{accounts?}` | `{ok, msg}` |
| POST | `/api/robots/manage` | `{action:"add"\|"remove", accounts}` | `{ok, msg}`（**可选鉴权**） |
| POST | `/api/robot/restart` | — | `{ok, msg}` 重启机器人进程（单进程）（**可选鉴权**） |
| POST | `/api/robot/reload_scripts` | `{modules?["quest_engine",…]}` | `{ok, modules, msg}` **脚本热更**：机器人进程内 `importlib.reload` 指定模块，免重启铺脚本补丁（省略 `modules` = 机器人端默认 `quest_engine`）；建议任务间隙触发（会重置模块级缓存）（**可选鉴权**） |
| WS | `/ws` | — | 实时事件推送（见 §4） |
| GET | `/api/autotask` | — | `{ok, tasks[newbie,ghost,hatch,shenbu], candidates{…}, pools{…}, hatch_sessions[], reghost[]}` **定时自动任务（只读）**：各套独立策略的状态（配置/启用/下一轮/最近 12 轮）+ 候选数（"该做但没在做"的号）+ 池视图（可用/在跑/目标/缺口）+ 卡死待恢复名单；口径见 [定时自动任务](../02-架构/定时自动任务.md) |
| POST | `/api/autotask/start` | `{kind, interval_sec?, jitter_sec?, batch_min?, batch_max?, max_online?, register_enabled?, register_count?, launch_delay_sec?, min_level?, balance_gate?}` | `{ok, state, msg}` 启动/改参数某套策略（不传的字段沿用上次；**可选鉴权**）。`min_level`/`balance_gate` 仅新日常用：`min_level` = 该策略等级门槛（shenbu 默认 40，**覆盖全局 `CTRL_SHARE_DAILY_MIN_LEVEL`**）；`balance_gate` = 神捕余额闸（**可显式传 0 关闭**；>0 时过滤"余额已知且 < 闸值"的号） |
| POST | `/api/autotask/stop` | `{kind}` | `{ok, state, msg}` 停止（同时撤销"等下发"队列；`kind=shenbu` 时给在跑的号补发 `share_daily_stop` 收工）（**可选鉴权**） |
| POST | `/api/autotask/run` | `{kind}` | `{ok, rounds[], state, msg}` 立即跑一轮（仍受同时在线上限）（**可选鉴权**） |
| GET | `/api/daily/overview` | — | `{ok, share_keys[], rows[{account,level,queue[{share_key,kind,done,limit,state}],current,order[]}]}` **分享日常轮转总览（只读）**：`kind` ∈ `ghost/newbie/shenbu/fenghuo`（认不出为空串）；`state` 原样透传机器人上报（大写）；数据源 = 机器人心跳 `daily` 块 + 意图表 + 中控会话记账；老版机器人没有 `daily` 块 → `queue=[]`、`current=""`（空值安全）；轮转顺序（`order`）P2 再填（拟 `fixed/random`） |
| POST | `/api/reghost/cancel` | `{account}` | `{ok, canceled, msg}` 取消该号的"卡死自动重登恢复"（**可选鉴权**） |
| WS | `/ws` | — | 实时事件推送（见 §4） |

> **骨架层不含业务接口**（账号库、抓鬼、游荡、配置页等）；需要时按
> [扩展指南](../02-架构/扩展指南.md) §2 添加，并同步本表与 `internal/api/protocols.go`。

## 2. 参数约定

- **`accounts` 三态**：省略 = 全部账号；`["robotA"]` 或 `"robotA,robotB"` = 指定账号；
  `[["robotA","pwd"]]` = 带密码（仅 `robots/manage add` 用）。
- **`/api/start` 的 `chain_id`**：请求里传「链文件名 id」（或任意链 ID）；
  从文件加载时，**下发用文件内声明的 `chain_id`**（响应与日志里的 `chain_id` 即该值）。
- **单进程语义**：控制通道只有一条，所有命令都发给**唯一**的机器人连接；
  `zone` 只出现在配置接口里（表示"目标区"），不参与命令路由。
- **`/api/config/apply`**：把该区 `ip/port/PROTOCOL_CODING` 写进机器人 `script/config.py`
  （只改这三个键、写前备份；`restart_robot=true` 时顺带重启机器人进程）。
  详见 [多区配置](../05-运维/多区配置.md)。
- **密码只从账号库取，且按服通用**：请求里没给密码时，按 `zone` 查 `zones[addr].password` → **同服（同 host）其它区** → 账号级 `password`；**取不到就明确失败**（没有统一/默认密码这回事）。
- **`accounts/verify` 两种模式**：带 `accounts` 同步返回；不带则从池按区批量（`scope` 见 [游戏服探测协议](游戏服探测协议.md) §6.1）。
- **失败必须给原因**：所有接口失败返回 `ok:false` + `msg`（例：`下发失败：机器人通道未连接`）。
- **下发类接口的 `ok`** = 是否成功送达机器人（不是业务成功）；`chain_source` 表示链数据来源。

## 3. 可选鉴权

启用方式（任选其一，优先级：CLI > 环境变量 > config.local.json）：

1. CLI：`--api-token <token>`
2. 环境变量：`API_TOKEN`
3. `config.local.json`：`{"api_token": "..."}`（占位符 `CHANGE_ME*` 视为未配置）

校验方式：请求头 `Authorization: Bearer <token>` 或 `X-API-Token: <token>`，不匹配返回 **401**。
**保护的高危写接口**：`logs/clear`、`robots/manage`、`robots/batch`、`robot/restart`、
`config/servers`、`config/zones`、`config/servers/delete`、`config/zones/delete`、`config/apply`、
`accounts/add`、`accounts/remove`、`accounts/create`、`accounts/verify`、`accounts/verify/cancel`
（以及使用方新增的破坏性接口）；读接口与 `start`/`stop`/`reset`/`config/switch` 不保护，避免前端 break。

## 4. WebSocket `/ws`

- 建立后持续收到机器人事件（与运行历史同源）；客户端可发任意文本保活（中控侧回 `pong`）。
- 中控侧每客户端独立发送队列（256 条）：**队列写满的慢客户端会被断开**，前端每 3 秒自动重连。
- 消息类型：

| type | 说明 |
|---|---|
| 机器人事件原样 | `robot_online` / `status_reply` 之外的多数事件都会推送 |
| `robot_pos_batch` | `{type, list:[{account,mapid,x,y}]}`，100ms 合并的位置推送 |
| 不推送 | `robot_pos`（改合并推送）、`level=debug/trace` 的 `log` |

## 5. 示例

```bash
# 状态总览
curl http://127.0.0.1:28082/api/status

# 启动链（指定账号）
curl -X POST http://127.0.0.1:28082/api/start \
     -H "Content-Type: application/json" \
     -d '{"chain_id":"newbie_full","accounts":["robotA","robotB"]}'

# 链列表 / 单链详情
curl "http://127.0.0.1:28082/api/chains"
curl "http://127.0.0.1:28082/api/chains?id=newbie_full"

# 添加机器人（带密码）
curl -X POST http://127.0.0.1:28082/api/robots/manage \
     -H "Content-Type: application/json" \
     -d '{"action":"add","accounts":[["robotA","123456"]]}'

# 启用鉴权后调高危接口
curl -X POST http://127.0.0.1:28082/api/robot/restart -H "X-API-Token: <token>"

# 批量验证当前区（默认：从账号池按区选"待验证"的号 → 返回 job）
curl -X POST http://127.0.0.1:28082/api/accounts/verify \
     -H "Content-Type: application/json" \
     -d '{"zone":"47.96.8.240:2300","scope":"unverified","limit":20}'
# 进度：curl ".../api/accounts/verify/job?id=<job_id>"    停止：POST ".../api/accounts/verify/cancel"

# 按可用性检索账号池（分页）
curl "http://127.0.0.1:28082/api/accounts?zone=47.96.8.240:2300&usable=1&keyword=robot0001&limit=20&offset=0"
```
