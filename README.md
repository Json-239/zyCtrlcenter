# zyCtrlcenter（机器人中控骨架 · Go）

一个**零第三方依赖**的机器人中控骨架：TCP 控制通道 + HTTP API/WebSocket + 日期级日志 + 进程管理，
配套 Vue 3 简易面板与**完整复刻的测试框架**。

> 参考实现：`F:\ZyBin\xm\2d-xiyou-server\robot\ctrlcenter`（Python/FastAPI，业务完整）。
> 本项目只保留**基础设施与测试框架**，业务模块（账号库/定时编排/游荡/抓鬼资格…）由使用方按需扩展，
> 对照与扩展方式见 [与参考实现对照](docs/06-参考/与参考实现对照.md)、[扩展指南](docs/02-架构/扩展指南.md)。

## 快速开始

```powershell
# 1) 启动中控（HTTP API :28082，控制通道 :27200；默认不接管机器人进程）
run_ctrlcenter.bat
#    需要中控拉起机器人：run_ctrlcenter.bat --auto-robot [--kill-robots]

# 2) 启动前端面板（首次 npm install）
cd web; npm install; npm run dev        # http://localhost:5273

# 3) 机器人端 config.py 的 ctrl_server_ip/port 指向本机 27200，启动机器人即可上线
```

## 端口（与参考项目错开）

| 端口 | 用途 |
|---|---|
| **28082** | 中控 HTTP API + WebSocket（`GET /` 只返回 JSON 提示） |
| **27200** | 机器人控制通道（TCP JSON-lines，单连接） |
| **5273** | 前端 dev server（vite proxy `/api`、`/ws` → 28082） |

可用 `--web-port` / `--ctrl-port` 覆盖；多开隔离见 [部署与启动](docs/05-运维/部署与启动.md) §5。

## 能力范围（骨架层）

- **多区配置（服 → 区，单进程切换式）**：维护多个候选区（如同一 IP 的 2400/2300），
  面板可增删改 / 切换当前区；「应用到机器人」把该区 `ip/port/编码` 写进机器人 `config.py`
  （只改三个键、写前备份），重启机器人即完成切区。见 [多区配置](docs/05-运维/多区配置.md)。
- **控制通道**：TCP JSON-lines、单连接接管、命令 5s 写超时（失败给原因）、事件带当前区标记。
- **事件处理**：表驱动，覆盖全部协议事件；状态映射（含区归属）+ 错误记账 + 自动下机。
- **日志（日期级）**：`data/runs_YYYYMMDD.jsonl`、`data/bot_logs/<账号>/runs_YYYYMMDD.log`、
  `logs/ctrlcenter_YYYYMMDD.log`；保留窗口清理 + 单文件熔断归档。
- **账号池 + 批量上下线**：导入 Python 中控账号库（5005 个号 + **按区可用性**），面板可筛选/勾选，
  批量上线（自动选可用号、跳过已在线、分批 + 间隔）与批量下线；「账号池」页见 [账号池](docs/05-运维/账号池.md)。
- **地图可视化页**：用真实阻挡网格画底图，按状态着色显示当前地图所有在线机器人，
  点击点位弹出详情（状态/坐标/HP·MP/**守护**/**背包**）。见 [坐标与地图](docs/05-运维/坐标与地图.md) §3。
- **HTTP API（24 + WS）**：status / **accounts（账号池）+ robots/batch（批量上下线）** /
  **config（多区配置）** / chains / **maps + map/grid（地图名与网格）** /
  logs / protocols / start / stop / reset / robots/manage / robot/restart / ws；高危写接口支持可选 token。
- **链数据**：文件驱动（`data/chains/<文件名>.json`）+ **未知字段与形状原样透传**；无文件时只下发 `chain_id`。
- **坐标与地图**：机器人上报的像素坐标自动换算成**客户端格子坐标**（`/16`，`pos_grid`）；
  `mapid` 经 `data/maps.json`（131 张图）显示成中文名。见 [坐标与地图](docs/05-运维/坐标与地图.md)。
- **进程管理**：单机器人进程按需拉起/停止/重启（默认不动外部进程）。
- **测试框架**：`test/` 目录约定 + 共享基座 + 真实报文夹具（provenance 强制校验）+ 每模块 README + 一键运行。

**不做**：业务决策（换号/冷却/资格/补位）、数据库、链数据生产、自动复制部署目录、HTML 面板托管
—— 见 [项目简介](docs/01-总览/项目简介.md) §边界。

## 目录结构（简）

```
zyCtrlcenter/
├── main.go                入口（装配 + 生命周期）
├── internal/              config / logging / store / state / ctrl / wsutil / chainlib / services{event,process} / api
├── test/                  测试（规范与基座见 docs/04-测试/）
├── tools/                 运维小工具（账号库查询 / 改部署目录账号段）
├── docs/                  文档库（唯一入口 docs/README.md）
├── data/                  运行时数据（data/chains 为可选资产，随项目入库）
├── logs/                  应用日志（不入库）
├── web/                   前端面板（Vue3 + Vite）
├── run_ctrlcenter.bat     编译 + 启动
└── run_tests.bat          一键测试（vet + test + 可选 race）
```

完整目录说明见 [目录结构](docs/01-总览/目录结构.md)。

## 测试

```powershell
run_tests.bat                          # go vet + 全量测试（+ 可选 race）
go test ./test/... -count=1            # 手动全量
go test ./test/accounts/ -v            # 账号池（导入/过滤/选号/热更新）
go test ./test/zones/ -v               # 多区配置（注册表 + 写机器人 config.py）
go test ./test/api/ -run TestRobotsBatchOnlineAutoPick -v      # 批量上线（自动选号 → 机器人实收）
```

- 用例分布与保障清单：[覆盖现状](docs/04-测试/覆盖现状.md)（当前 11 个模块 / **89 用例**）
- 规范（目录/铁律/README 模板）：[测试规范](docs/04-测试/测试规范.md)
- 基座与夹具：[测试基座](docs/04-测试/测试基座.md)、[夹具规范](docs/04-测试/夹具规范.md)

## 文档索引

**文档唯一入口：[docs/README.md](docs/README.md)**（按 01-总览 / 02-架构 / 03-协议 / 04-测试 / 05-运维 / 06-参考 分域）。

| 先看这些 | 说明 |
|---|---|
| [项目简介](docs/01-总览/项目简介.md) | 定位、边界、关键特性 |
| [快速开始](docs/01-总览/快速开始.md) | 3 步跑起来 |
| [分层与模块](docs/02-架构/分层与模块.md) | 架构与依赖方向 |
| [控制通道协议](docs/03-协议/控制通道协议.md) / [HTTP-API](docs/03-协议/HTTP-API.md) | 事实协议 |
| [多区配置](docs/05-运维/多区配置.md) | 服 → 区模型、切换 vs 应用、每区独立部署目录 |
| [部署与启动](docs/05-运维/部署与启动.md) / [配置说明](docs/05-运维/配置说明.md) / [日志与排障](docs/05-运维/日志与排障.md) | 运维 |
| [与参考实现对照](docs/06-参考/与参考实现对照.md) | 复刻了什么、没复刻什么 |
