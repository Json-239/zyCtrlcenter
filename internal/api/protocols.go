package api

import (
	"net/http"

	"zyctrlcenter/internal/config"
)

// handleProtocols 协议映射（只读）：面板「协议映射」页数据源。
// 事实协议表单一事实源在 docs/03-协议/；改协议时两处一起更新。
func (a *API) handleProtocols(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"updated": "2026-09-21",
		"current": map[string]any{
			"version":      config.Version,
			"coding":       "UTF-8 JSON-lines（**上行**每行 ≤64KB；下行命令可带 2MB 级链数据载荷）",
			"web_port":     a.Cfg.WebPort,
			"ctrl_addr":    a.Ctrl.Addr(),
			"zones":        len(a.Zones.Flat()),
			"current_zone": a.currentZoneKey(),
			"connected":    a.Ctrl.Connected(),
			"robot_exe":    a.Cfg.RobotExe,
		},
		"sections": []map[string]any{
			{
				"key": "cmd", "title": "中控 → 机器人（命令）",
				"desc":    "TCP 控制通道下行命令（cmd 字段），骨架层只做原样透传",
				"columns": []string{"命令", "关键载荷", "说明"},
				"rows": [][]string{
					{"start_chain", "chain_id + chain{…}? + accounts?", "启动/续跑链（chain 可省略，由机器人端决定）"},
					{"ghost_start", "chain_id + chain{…} + role:solo + daily_limit + done? + accounts", "钟馗抓鬼日常：**必须带导航数据**（npcs/map_grids/dijkstra/ghost_maps/ghost_map_pos），不带 = 机器人原地不动；载荷由中控组装（基座链坐标/网格/路由 + 抓鬼专属刷鬼图/落点，口径同参考实现 ghost_nav_payload），缺失或地址不全即硬失败（一条命令都不发）"},
					{"ghost_stop", "—", "停止抓鬼（机器人保持在线）"},
					{"stop", "accounts?", "停链（机器人保持在线）"},
					{"reset", "accounts?", "重置重跑"},
					{"status", "—", "心跳（中控每 3 秒下发，机器人回 status_reply）"},
					{"robot_manage", "action: add|remove + accounts", "动态添加/移除账号（add 携带 [账号,密码]）"},
				},
			},
			{
				"key": "evt", "title": "机器人 → 中控（事件）",
				"desc":    "TCP 控制通道上行事件（type 字段）→ 状态表 + 运行历史 + WS 推送",
				"columns": []string{"事件", "关键字段", "处理"},
				"rows": [][]string{
					{"hello", "robot_version / pid", "记日志（握手）"},
					{"robot_online", "account / role_name / level / mapid", "清已移除标记、建行置 ONLINE、落历史"},
					{"robot_offline", "account", "置 OFFLINE、清握手标记"},
					{"status_reply", "server / robots[{account,state,task_index,done,chain_done,level,mapid,pos,hp,hs,err_code,err_ts,…}]", "更新状态表 + 记录游戏服地址（不落历史，量太大）"},
					{"robot_manage_reply", "result:{added,removed}", "落历史"},
					{"task_progress", "account / done / task_index", "更新进度 + 落历史"},
					{"robot_state", "account / state / pos / hp / mp / bag / booth", "更新快照 + 落历史"},
					{"robot_pos", "account / mapid / x / y", "更新位置；100ms 合并为 robot_pos_batch 推 WS"},
					{"chain_done", "account / already_done?", "置 DONE；AutoRemoveOnDone 且非 already_done 时下机"},
					{"ghost_done", "account / done / limit / reason", "落历史（业务处置由使用方实现）+ 可选下机"},
					{"ghost_offline", "account / code / reason", "落历史（业务处置由使用方实现）+ 可选下机"},
					{"error", "account / code / msg", "记 err_code/err_repeat（面板「任务失败待处理」）+ 落历史"},
					{"log", "account? / level / msg", "落历史；debug/trace 不推 WS"},
					{"pong", "—", "无处理"},
				},
			},
			{
				"key": "api", "title": "中控 → 前端（HTTP API）",
				"desc":    "面板使用的全部接口（完整表见 docs/03-协议/HTTP-API.md）",
				"columns": []string{"方法", "路径", "返回概要"},
				"rows": [][]string{
					{"GET", "/api/status", "机器人表（含区，行带 paused/restore_capped+cap_until+cap_reason 等标记）+ 通道/进程状态 + 当前区 + 失败清单 + 卡死熔断名单 restore_capped[]"},
{"GET", "/api/autotask", "定时自动任务（各套独立策略：新手链/抓鬼/孵化/大唐神捕）+ 候选数 + 待恢复名单（reghost[]：含 waiting_* / done / failed / capped=当日卡死熔断等次日）"},
{"POST", "/api/autotask/start|stop|run", "启停某套定时任务 / 立即跑一轮（可选鉴权）"},
{"GET", "/api/daily/overview", "分享日常轮转总览（号 × 日常 × 进度；数据源=心跳 daily 块 + 意图/池；轮转顺序 P2 再填）"},
					{"POST", "/api/reghost/cancel", "取消某号的卡死自动重登恢复（可选鉴权）"},
					{"POST", "/api/reghost/resume", "{account?} 解除「当日卡死熔断」（省略=全部）：清熔断表+当日卡死计数+重登记录，号回到自动通道（跨日 0 点本会自动解除）（可选鉴权）"},
					{"GET", "/api/tasknames?id=", "任务号 → 任务名（读游戏配置 task/*.xml；面板把编号翻成人话）"},
{"GET", "/api/modules", "模块地图（只读）：模块注册表（动作 9 + 链数据 2 + 编排 1）+ 最近一次测试报告（缺失显示—）"},
					{"GET", "/api/chains?id=", "链列表（文件驱动）+ 指定链详情"},
					{"GET", "/api/logs?n=200", "运行历史尾部"},
					{"POST", "/api/logs/clear", "清空运行日志（可选鉴权）"},
					{"GET", "/api/protocols", "本协议映射（只读）"},
					{"GET", "/api/accounts", "账号池（含各区的 verified/usable/verify_msg）+ 统计，支持关键词检索"},
					{"POST", "/api/accounts/add|remove", "加号入池 / 从池删除（可选鉴权）"},
					{"POST", "/api/accounts/create", "{accounts, zone?, password_len?, interval_ms?} 建号（注册协议 106→104→700）；密码随机生成并写入账号库（可选鉴权）"},
					{"POST", "/api/accounts/verify", "账号可用性验证（直连游戏服 102 探测；可选鉴权）。带 accounts=同步验这几个；不带=**默认从账号池按区批量验**（scope/limit/skip_online），返回 job"},
					{"GET", "/api/accounts/verify/job", "批量验证进度（?id=，缺省最近一个）：status/total/done/usable/unusable/not_exists/error/results"},
					{"POST", "/api/accounts/verify/cancel", "{id?} 停止批量验证（在途结果丢弃，不写回池）（可选鉴权）"},
					{"POST", "/api/robots/batch", "{action:online|offline, accounts?, zone?, limit, chunk, interval_ms} 批量上线/下线（按区，可选鉴权）"},
					{"GET", "/api/accounts/stats", "?zone= 账号池统计 + pending{zone,unverified,unusable,unknown,all}（面板显示『本次将验 N 个』）"},
					{"GET", "/api/intents", "账号意图表（只读）：{newbie_max_level, counts, intents[{account,kind,chain_id,zone,source,reason,since}], auto_restore, recover{account:{attempts,next_at,blocked,last_msg}}}（等级<31→新手链优先，≥31 或已完成→抓鬼；等级未知不登记）"},
					{"POST", "/api/intents/restore", "{account?} 立即按意图补发一次（start_chain/ghost_start，**同命令+同链合并成一条**）：冷却/熔断仍生效，跳过在跑/跑完/离线；返回 sent=账号数、commands=命令数（可选鉴权）。抓鬼号卡 DIALOG 且无活跃抓鬼会话也算作没在跑 → 补发（2026-09-21）。{account} 指定时**顺带解除该号当日卡死熔断**（uncapped）；不带 account 时被熔断拦下的号列在 capped_skipped（2026-09-23）"},
					{"GET", "/api/config", "多区配置（服/区列表 + 当前区 + 机器人 config.py 路径）"},
					{"POST", "/api/config/switch", "{server?, zone | key} 切换当前区（低风险）"},
					{"POST", "/api/config/servers", "{server:{…,zones:[…]}} 新增/更新服（可选鉴权）"},
					{"POST", "/api/config/zones", "{server, zone:{…}} 新增/更新区（可选鉴权）"},
					{"POST", "/api/config/servers|zones/delete", "删除服/区（可选鉴权）"},
					{"POST", "/api/config/apply", "{zone?, restart_robot?} 写机器人 config.py（可选鉴权）"},
					{"POST", "/api/start", "{chain_id?, chain?, accounts?, auto?} 启动链：auto=true 按意图自动分配（新手链→start_chain 带链数据；抓鬼→ghost_start 带导航数据）；非 auto 手选导航数据（无任务节点）直接拒绝；条件不匹配回带 warnings[{account,msg}]"},
					{"POST", "/api/stop|reset", "{accounts?} 停链/重置"},
					{"POST", "/api/robots/manage", "action add/remove（可选鉴权）"},
					{"POST", "/api/robot/restart", "重启 robot.exe（可选鉴权）"},{"POST", "/api/robot/reload_scripts", "{modules?[\"quest_engine\",…]} 脚本热更：机器人进程内 importlib.reload，免重启铺补丁（可选鉴权）"},
					{"WS", "/ws", "实时事件推送（事件带 zone = 当前区）"},
				},
			},
			{
				"key": "game", "title": "中控 → 游戏服（账号探测协议）",
				"desc": "账号可用性验证专用：中控开一条独立 TCP，跑一遍登录协议（不真正进入游戏，无副作用）。" +
					"帧 = [type int32 LE][pblen int32 LE][body]；字符串 = [len int32 LE][编码字节]。完整说明见 docs/03-协议/游戏服探测协议.md",
				"columns": []string{"消息", "方向", "说明"},
				"rows": [][]string{
					{"100 MC_CONNECT", "C2S", "握手（空体）"},
					{"200 S2C_CONNECT_QUERY", "S2C", "服务端连接查询 → 客户端回 255[200]"},
					{"255 MS_CONNECT_BACK", "双向", "握手回包；收到后客户端发 101[版本号]"},
					{"101 MC_VER", "C2S", "版本验证（版本号字符串，默认 58740022）"},
					{"400 S2C_VER", "S2C", "版本返回 [retcode, msg]；retcode=0 才继续"},
					{"102 MC_LOGIN_LANGQI", "C2S", "登录 [账号, md5(明文密码)]（md5 小写 hex）"},
					{"300 S2C_ACK_ACCOUNT", "S2C", "登录验证返回 [retcode, msg]：0=成功 / 100=账号不存在 / 200=密码错 / 300=冻结"},
					{"1000 C2S_QUERY_ROLE", "C2S", "查角色（仅登录成功且 query_role=true 时发）"},
					{"90132 S2C_QUERY_ROLE", "S2C", "角色列表 [数量, (role_id, role_index, 角色名, 等级, 转生, 外观×4, contour)]"},
				},
			},
		},
	})
}
