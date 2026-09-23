package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/chainplan"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/restorer"
	"zyctrlcenter/internal/wsutil"
)

// ---------------------------------------------------------------- 基础

func (a *API) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "not found: " + r.URL.Path})
}

func (a *API) handleIndex(w http.ResponseWriter, r *http.Request) {
	// 走到这里说明 web/dist 未构建（否则 handleRoot 已直接返回面板首页）。
	// 面板地址口径：构建后用 http://<host>:<web-port>/ 直连；未构建时用 vite dev（5273）。
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     "zyctrlcenter 中控 API",
		"version":  config.Version,
		"frontend": "web（Vue3 + Vite）",
		"panel":    "未构建：运行 start.bat（Windows）/ ./start.sh（Linux/macOS）一键构建并启动；开发模式 cd web && npm install && npm run dev（http://localhost:5273）",
		"api":      "/api/status",
		"ws":       "/ws",
		"ports":    map[string]any{"web": a.Cfg.WebPort, "ctrl": a.Cfg.CtrlPort},
	})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	online, handshake, total := a.St.Counts()
	serverKey, zoneKey := a.Zones.CurrentKeys()
	// 2026-09-22 全服在线（含真实玩家）：中控直连游戏服 /gm/online（livecount，source=svr_provider）
	// 优先；其次机器人 @online 回执（source=svr）；都没有时 source=local（面板/水位保持器会用
	// 本地握手数兜底）。count/ts/age_sec 字段语义不变，仅新增 source 标明来源。
	svrOnline := map[string]any{"source": "local"}
	if c, tsMS, source, ok := a.svrOnlineReading(); ok {
		ageSec := 0.0
		if tsMS > 0 {
			ageSec = float64(time.Now().UnixMilli())/1000.0 - tsMS/1000.0
			if ageSec < 0 {
				ageSec = 0
			}
		}
		svrOnline = map[string]any{"count": c, "ts": int64(tsMS), "age_sec": int(ageSec), "source": source}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                      true,
		"version":                 config.Version,
		"robots":                  a.St.Snapshot(),
		"counts":                  map[string]any{"total": total, "online": online, "handshake": handshake},
		"ghost_unavailable_today": a.St.GhostUnavailableTodayCount(), // 今日已标"抓鬼不可用/已满"的号数
		"svr_online":              svrOnline,
		"zones":                   a.zoneSnapshots(), // 全部候选区（含 current 标记）
		"current":                 a.currentZone(),
		"current_keys":            map[string]any{"server": serverKey, "zone": zoneKey},
		"zone_counts":             a.St.ZoneCounts(),
		"removed":                 a.St.RemovedList(),
		"paused":                  a.St.PausedList(), // 2026-09-23 R3：人工暂停名单（停止按钮打标）
		"robot_connected":         a.Ctrl.Connected(),
		"ctrl_addr":               a.Ctrl.Addr(),
		"ctrl_zone":               a.Ctrl.Tag(),
		"robot_running":           a.Proc.Running(),
		"robot_exe":               a.Cfg.RobotExe,
		"robot_exe_exists":        a.Proc.ExeExists(),
		"server":                  a.St.CurServer(zoneKey), // 当前区机器人上报的游戏服地址
		"waterline":               a.waterlineSnapshot(),   // 在线水位保持器摘要（面板少一次请求）
		"livecount":               a.liveCountSnapshot(),   // 服务端在线数直连数据源诊断（默认关闭）
		"task_failed":             a.St.FailedTasks(2, 30),
		"chains":                  len(a.chainInfo()),
		"maps_count":              a.Maps.Count(), // 地图名表条目数（表在 /api/maps）
		"grid_cell":               a.Cfg.GridCell, // 客户端坐标口径：pos_grid = pos / grid_cell
		"ws_clients":              a.WS.Count(),
		"logs_file":               a.Store.CurrentPath(),
		"data_dir":                a.Cfg.DataDir,
		"chain_dir":               a.Cfg.ChainDir,
		"zones_file":              a.Zones.Path(),
		"deploy_dir":              a.Cfg.DeployDir,
	})
}

// handleChains 链列表（文件驱动）；带 ?id= 时附上该链完整数据。
func (a *API) handleChains(w http.ResponseWriter, r *http.Request) {
	raw := a.chainInfo()
	items := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		items = append(items, map[string]any{
			"id": it.ID, "chain_id": it.ChainID, "name": it.Name,
			"start_task": it.StartTask, "end_task": it.EndTask,
			"task_count": it.TaskCount, "file": it.File, "error": it.Error,
			// 没有任务节点 = 导航数据（如钟馗抓鬼日常），**不是可启动的链**（面板据此标注/过滤）
			"nav_only": it.TaskCount == 0,
		})
	}
	resp := map[string]any{"ok": true, "chain_dir": a.Cfg.ChainDir, "chains": items}
	if id := r.URL.Query().Get("id"); id != "" {
		chain, err := chainlib.Build(id, a.Cfg.ChainDir)
		switch {
		case err == nil:
			resp["chain"] = chain
			resp["chain_found"] = true
			// 模块化组装（只读视图，中控不执行）：把提示表展开成模块序列 + 结构校验
			plan, perr := chainplan.Build(chain)
			resp["plan"] = plan
			if perr != nil {
				resp["plan_error"] = perr.Error()
			}
		case errors.Is(err, chainlib.ErrNotFound):
			resp["chain_found"] = false
			resp["msg"] = "链数据文件不存在（" + chainlib.FilePath(id, a.Cfg.ChainDir) +
				"）；启动时只会下发 chain_id，由机器人端决定怎么跑"
		default:
			resp["ok"] = false
			resp["chain_found"] = false
			resp["msg"] = "链数据解析失败: " + err.Error()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleIntents 账号意图表（只读）：每个账号"该跑哪条链"（新手链/抓鬼/捉鬼/空闲）。
//
//	GET /api/intents
//
// 口径：等级 ≥ newbie_max_level(默认 31) 或新手链已完成 → 抓鬼；等级 < 31 → 新手链优先；
// 等级未知 → 不登记（等机器人上线报一次等级）。一个账号同一时刻只有一条链。
// P0 只记账；P1 会按这张表补发 start_chain/ghost_start（掉线/重启恢复）。
func (a *API) handleIntents(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"ok": true, "count": 0, "intents": []any{}, "counts": map[string]int{}}
	if a.Events == nil || a.Events.Intents == nil {
		resp["msg"] = "意图表未启用"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	plan := a.Events.Intents
	dec := plan.Decider()
	items := plan.Snapshot()
	resp["count"] = len(items)
	resp["intents"] = items
	resp["counts"] = plan.Counts()
	resp["newbie_max_level"] = dec.NewbieMaxLevel
	resp["newbie_chain_id"] = dec.NewbieChainID
	resp["zhuaogui_chain_id"] = dec.ZhuaoguiChainID
	resp["auto_restore"] = a.Cfg != nil && a.Cfg.AutoRestore
	if a.Restorer != nil {
		resp["recover"] = a.Restorer.Status() // 每账号：尝试次数/下次时间/熔断（面板显示"在补哪几个"）
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleIntentsRestore 立即按意图补发一次（面板「立即补发」；总开关关着也能手动触发）。
//
//	POST /api/intents/restore
//	{} 或 {"account": "a@x.com"}  （省略 = 全表）
//
// 冷却/熔断仍然生效（不会对着卡死的号猛发）；命令经控制通道下发，action 记为 restore_*。
//
// 同命令 + 同链的号**合并成一条**命令下发（抓鬼载荷是 2MB 级导航数据，一个号一条 = N×2MB）；
// 因此回带 `commands`（实际下发条数）与 `sent`（覆盖的账号数）两个数。
func (a *API) handleIntentsRestore(w http.ResponseWriter, r *http.Request) {
	if a.Restorer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "恢复引擎未启用"})
		return
	}
	body := readBody(r)
	only := strings.TrimSpace(toStr(body["account"]))
	// 2026-09-23 R3：手动「立即补发」= 用户要它跑 → 先解除人工暂停（单个/全部），
	// 再跑 TickForce；否则暂停闸（GhostSkipFunc）会把它们全部拦下（"补发没用"观感）。
	if only != "" {
		a.resumeAccounts([]string{only})
	} else {
		a.resumeAccounts(nil)
	}
	acts := a.Restorer.TickForce(time.Now(), true)

	// 先按账号过滤（每个号单独点「补发」时只处理它），再合并 —— 顺序反了会把别的号也带出去
	kept := make([]restorer.Action, 0, len(acts))
	skipped := 0
	for _, act := range acts {
		if only != "" && act.Account != only {
			skipped++
			continue
		}
		kept = append(kept, act)
	}

	sentAccounts, commands, skippedGroups := 0, 0, 0
	actions := make([]map[string]any, 0, len(kept))
	for _, g := range restorer.GroupActions(kept) {
		cmd, err := a.restoreCommand(g)
		var ok bool
		if err != nil {
			skippedGroups++ // 载荷取不到（文件被删/改坏）：不发空命令，逐号标注原因
		} else {
			ok = a.Events != nil && a.Events.SendCmd(cmd, "restore_"+g.Command)
			if ok {
				commands++
				sentAccounts += len(g.Accounts)
			}
		}
		for _, acc := range g.Accounts {
			item := map[string]any{
				"account": acc, "command": g.Command, "chain_id": g.ChainID,
				"reason": g.Reason, "sent": ok,
			}
			if err != nil {
				item["msg"] = "载荷不可用，未补发：" + err.Error()
			}
			actions = append(actions, item)
		}
	}

	msg := "已按意图补发（冷却/熔断仍然生效；没列出来的号=没意图/在跑/冷却中）"
	if skippedGroups > 0 {
		msg = "部分号未补发：载荷不可用（" + itoa(skippedGroups) + " 批），详情见 actions[].msg"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "sent": sentAccounts, "commands": commands, "skipped": skipped,
		"actions": actions, "msg": msg,
	})
}

// restoreCommand 一批补发 → 一条下行命令（含按 kind 补充的载荷；取不到返回 error）。
func (a *API) restoreCommand(g restorer.Group) (map[string]any, error) {
	cmd := map[string]any{"cmd": g.Command, "accounts": g.Accounts}
	if g.ChainID != "" {
		cmd["chain_id"] = g.ChainID
	}
	payload, err := a.chainPayloads().For(g.Kind, g.ChainID)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		cmd["chain"] = payload
		// 载荷自带 chain_id（链数据文件里声明/按文件名补的）→ 顶层也带上，面板日志对得上
		if ch, ok := payload.(*chainlib.Chain); ok && ch.ChainID != "" && cmd["chain_id"] == nil {
			cmd["chain_id"] = ch.ChainID
		}
		if g.Command == "ghost_start" {
			// 与「启动」自动分配到抓鬼时同口径（参考实现 intent_restore 也是 role+limit+chain）
			cmd["role"] = "solo"
			cmd["daily_limit"] = a.chainPayloads().GhostDailyLimit()
		}
	}
	return cmd, nil
}

// handleMaps 地图名表（mapid → 中文名）+ 客户端坐标口径；面板缓存后用 mapLabel/posLabel 显示。
func (a *API) handleMaps(w http.ResponseWriter, r *http.Request) {
	maps := a.Maps.All()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"count":     len(maps),
		"grid_cell": a.Cfg.GridCell, // 客户端坐标 = 像素 / grid_cell
		"source":    a.Maps.Path(),
		"maps":      maps,
	})
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		n = toInt(v, 200)
	}
	if n <= 0 || n > 5000 {
		n = 200
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": a.Store.ReadTail(n)})
}

func (a *API) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	res := a.Store.ClearAll()
	a.Store.LogEvent(map[string]any{"type": "api", "action": "logs_clear",
		"removed_runs": res["removed_runs"], "removed_bot": res["removed_bot"]})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "removed_runs": res["removed_runs"], "removed_bot": res["removed_bot"],
		"msg": "已清空运行日志（历史 runs / bot 日志）",
	})
}

// ---------------------------------------------------------------- 机器人控制（单通道）

// handleStart 启动任务链。
//
// 链数据的三种来源（优先级）：
//  1. 请求体直接给 `chain`（原样透传）；
//  2. <数据目录>/chains/<chain_id>.json 文件；
//  3. 都没有 → 只下发 chain_id（不带 chain 字段）。
func (a *API) handleStart(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	chainID := toStr(body["chain_id"])
	if chainID == "" {
		chainID = a.Cfg.DefaultChainID
	}
	accounts := bodyAccounts(body)

	// 2026-09-23 R3：用户显式「启动」= 解除人工暂停（空账号 = 全部解除，与"停全部"对称）。
	// 放在最前面：后面的 auto 分支/直派都按"用户就是要跑"执行。
	if resumed := a.resumeAccounts(accounts); len(resumed) > 0 {
		a.Log.Printf("[START] 解除人工暂停 %d 个：%v", len(resumed), resumed)
	}

	// auto=true：不手选链，**按账号意图自动分配**（新手链 → start_chain；抓鬼 → ghost_start；无意图 → 回落指定链）
	if toBool(body["auto"], false) {
		a.startAuto(w, chainID, accounts, body["chain"])
		return
	}

	// 防误选（接口层兜底，面板同时禁用「启动链」按钮）：导航数据（没有任务节点）不是链，
	// 下发 start_chain 只会让机器人拿到空 task_order 干等 —— 直接拒绝，不静默。
	// 文件不存在仍然走「只下发 chain_id」的老路（那是正常的"链数据由机器人端自备"用法）。
	if body["chain"] == nil {
		if navOnly, err := a.chainPayloads().NavOnly(chainID); err == nil && navOnly {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "nav_only": true, "chain_id": chainID,
				"msg": "「" + chainID + "」是导航数据（没有任务节点），不能作为任务链启动；抓鬼请走「启动」自动分配（ghost_start）",
			})
			return
		}
	}

	cmd := map[string]any{"cmd": "start_chain", "chain_id": chainID}
	source := "none"
	switch {
	case body["chain"] != nil:
		cmd["chain"] = body["chain"]
		source = "request"
	default:
		chain, err := chainlib.Build(chainID, a.Cfg.ChainDir)
		if err == nil {
			cmd["chain"] = chain
			source = "file"
			if chain.ChainID != "" {
				chainID = chain.ChainID
				cmd["chain_id"] = chainID
			}
		} else if !errors.Is(err, chainlib.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "链数据解析失败: " + err.Error()})
			return
		}
	}
	if len(accounts) > 0 {
		cmd["accounts"] = accounts
	}

	ok := a.Events.SendCmd(cmd, "start_chain")
	a.Store.LogEvent(map[string]any{"type": "api", "action": "start", "zone": a.currentZoneKey(),
		"chain_id": chainID, "accounts": accounts, "chain_source": source, "sent": ok})
	msg := okMsg(ok, "已下发启动链")
	switch source {
	case "file":
		msg += "（链数据来自 " + chainlib.FilePath(chainID, a.Cfg.ChainDir) + "）"
	case "none":
		msg += "（未找到链数据文件，仅下发 chain_id）"
	}
	warnings := a.conditionWarnings(chainID, accounts)
	if len(warnings) > 0 {
		msg += "；" + warningsText(warnings)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "msg": msg, "chain_id": chainID, "chain_source": source,
		"warnings": warnings,
	})
}

// conditionWarnings 选了具体链但账号当前条件"不该跑这条"时给出人话提示（不阻断下发）。
//
// 现状只覆盖一种最容易踩的：该号已判抓鬼（≥31 或链已完成），却手选了剧情链 ——
// 机器人端会按 already_done 直接跳过（看起来"点了没反应"），提示改用「自动分配」。
// accounts 为空（=全部号）时不猜，返回空。
func (a *API) conditionWarnings(chainID string, accounts []string) []map[string]any {
	out := []map[string]any{}
	if len(accounts) == 0 {
		return out
	}
	kinds, reasons := a.intentKinds()
	for _, acc := range accounts {
		kind, reason := a.decideKind(acc, kinds, reasons)
		if kind != intent.KindGhost {
			continue
		}
		out = append(out, map[string]any{
			"account": acc, "command": "ghost_start", "chain_id": chainID,
			"msg": acc + " 当前条件应分配抓鬼（" + reason + "）；本次下发的是 " + chainID +
				"，机器人端可能直接判 already_done 不跑 —— 建议用「自动分配」或抓鬼入口",
		})
	}
	return out
}

// ghostDoneMap 已抓次数（账号 → 今日已抓）：取运行时状态里机器人上报的 ghost.done。
// 带上它，重新下发抓鬼不会把进度抹掉（参考实现 routers/ghost.py 的 done_map 同款）；
// 没有值就不带（机器人会走"服务端 task_limited 校准"分支，也能对）。
func (a *API) ghostDoneMap(accounts []string) map[string]int {
	out := map[string]int{}
	if a.St == nil {
		return out
	}
	for _, acc := range accounts {
		r, ok := a.St.Get(acc)
		if !ok || r.Ghost == nil {
			continue
		}
		// 2026-09-23 跨夜修复：旧快照（count_date 非今天）的 done 不下发 —— 服务端
		//   0 点已清零，旧值会让机器人误判满额（机器人端也会忽略，这里双保险）。
		if cd := toStr(r.Ghost["count_date"]); cd != "" && cd != todayKey() {
			continue
		}
		if n := toInt(r.Ghost["done"], 0); n > 0 {
			out[acc] = n
		}
	}
	return out
}

// warningsText 把 warnings 拼成一行（复用既有的 toast 通道显示给用户）。
func warningsText(warnings []map[string]any) string {
	msgs := make([]string, 0, len(warnings))
	for _, w := range warnings {
		if s := toStr(w["msg"]); s != "" {
			msgs = append(msgs, s)
		}
	}
	return strings.Join(msgs, "；")
}

// startAuto 按意图分配链路并分组下发（大屏「启动」默认走这里，不用先手选链）。
//
// 分组规则（判据见 internal/services/intent）：
//
//	意图=新手链/捉鬼链 → start_chain（带链数据，链数据来源同 handleStart）
//	意图=抓鬼        → ghost_start（钟馗抓鬼日常，不是剧情链）
//	没有意图（等级未知/未登记）→ 回落请求给的 chain_id（不乱猜）
//	意图=空闲        → 不发
func (a *API) startAuto(w http.ResponseWriter, defaultChainID string, accounts []string, inlineChain any) {
	kinds, reasons := a.intentKinds()
	targets := append([]string(nil), accounts...)
	if len(targets) == 0 { // 没给账号 = 全部：意图表里的 + 运行时在线但没登记意图的
		seen := map[string]bool{}
		for acc := range kinds {
			targets, seen[acc] = append(targets, acc), true
		}
		if a.St != nil {
			for _, rb := range a.St.Snapshot() {
				if rb.Account != "" && !seen[rb.Account] {
					targets, seen[rb.Account] = append(targets, rb.Account), true
				}
			}
		}
		sort.Strings(targets)
	}

	// 意图表里还没有这个号时（刚 add、还没报等级），回退用**账号池里的当前条件**判一次：
	// ≥31 级或链已完成 → 抓鬼；<31 → 新手链。这样"启动自动分配"永远按当前条件分配**一条**，
	// 而不是盲目发默认链（≥31 的号发 newbie_full 会被机器人端按 already_done 跳过 = 看起来"没分配"）。
	var toChain, toGhost []string
	skipped := []string{} // 等级门槛拦下的号（不静默：回带原因）
	assignments := make([]map[string]any, 0, len(targets))
	for _, acc := range targets {
		kind, reason := a.decideKind(acc, kinds, reasons)
		switch kind {
		case intent.KindIdle:
			continue
		case intent.KindGhost:
			// 抓鬼等级门槛（口径同参考实现 ghost_fit）：等级已知且低于门槛就不派 ——
			// 服务端会拒，硬派只会让机器人去空点钟馗（生产实测连点 15 次 → 重登风暴）。
			if ok, why := a.ghostGateFor(acc); !ok {
				skipped = append(skipped, acc+"（"+why+"）")
				assignments = append(assignments, map[string]any{
					"account": acc, "command": "", "reason": "不派抓鬼：" + why})
				continue
			}
			toGhost = append(toGhost, acc)
			assignments = append(assignments, map[string]any{
				"account": acc, "command": "ghost_start", "reason": "抓鬼（" + reason + "）"})
		default:
			toChain = append(toChain, acc)
			why := "未登记意图且池内无等级 → 用指定链"
			if reason != "" {
				why = "新手链（" + reason + "）"
			}
			assignments = append(assignments, map[string]any{
				"account": acc, "command": "start_chain", "chain_id": defaultChainID, "reason": why})
		}
	}

	// 2026-09-23 P0：池配额截断 —— "启动(自动分配)"不再无上限直派。
	// 生产实测（docs/04-测试/分析-20260923-抓鬼分配逻辑.md）：11:49:55 / 13:10:11 两次各带
	// 231 个号，按意图直派 227/225 个 ghost_start，把抓鬼会话推到 200+（目标 100）；
	// 与批量上线、RESTORE 补发叠加后长期超编 118。口径：max(0, 目标 - 在跑 - 在途)。
	// 手动通道特例（2026-09-23 调整）：池停用**不拦**手动（用户点了启动就是要跑），
	// 但仍按目标截断（在跑+在途已达标 → 不派，超编不该手动再加）。
	cutByQuota := func(accs []string, kind autotask.Kind, label string) []string {
		cut := a.cutByPoolQuota(accs, kind, true) // manual=true：见上（池停用放行、配额仍生效）
		if len(cut) == 0 {
			return accs
		}
		keep := len(accs) - len(cut)
		set := make(map[string]bool, len(cut))
		for _, acc := range cut {
			set[acc] = true
		}
		for i := range assignments {
			if set[toStr(assignments[i]["account"])] {
				assignments[i]["command"] = ""
				assignments[i]["reason"] = label + "池配额已满（在跑+在途 ≥ 目标），本次不派（等池内号收工或调大 target）"
			}
		}
		skipped = append(skipped, fmt.Sprintf("%d 个（%s池配额拦下）", len(cut), label))
		a.Log.Printf("[START] %s池配额拦下 %d 个（保留 %d 个，目标/在跑/在途见池状态）：%s",
			label, len(cut), keep, strings.Join(cut, ", "))
		if keep <= 0 {
			return nil
		}
		return accs[:keep]
	}
	toGhost = cutByQuota(toGhost, autotask.KindGhost, "抓鬼")
	toChain = cutByQuota(toChain, autotask.KindNewbie, "新手")

	// 2026-09-23 R1/R2（操作健壮性审计 · 连点去重）：去掉"最近 120 秒内已派发过"的号 ——
	// 抓鬼原先只在配额不满时才会重复派（配额有余就重发命令 → 机器人端重启会话）；
	// 新手链原先完全没有在途记账（连点必重复发 start_chain）。命中即在 assignments
	// 回带原因（不静默）；确需重发请等 TTL 过去，或用「重置重跑」。
	dropInflight := func(accs []string, kind autotask.Kind, label string) []string {
		var kept, dropped []string
		if kind == autotask.KindGhost {
			kept, dropped = a.ghostInflight.dropFresh(accs)
		} else {
			kept, dropped = a.chainInflight.dropFresh(accs)
		}
		if len(dropped) == 0 {
			return accs
		}
		set := make(map[string]bool, len(dropped))
		for _, acc := range dropped {
			set[acc] = true
		}
		for i := range assignments {
			if set[toStr(assignments[i]["account"])] {
				assignments[i]["command"] = ""
				assignments[i]["reason"] = label + "最近 120 秒内已派发（在途），本次不重复派；确需重发请稍候或先「重置重跑」"
			}
		}
		skipped = append(skipped, fmt.Sprintf("%d 个（%s最近已派发，去重跳过）", len(dropped), label))
		a.Log.Printf("[START] %s池最近已派发（在途），去重跳过 %d 个：%s",
			label, len(dropped), strings.Join(dropped, ", "))
		return kept
	}
	toGhost = dropInflight(toGhost, autotask.KindGhost, "抓鬼")
	toChain = dropInflight(toChain, autotask.KindNewbie, "新手")

	// 抓鬼载荷**在发任何命令之前**取齐：导航数据缺失就是硬失败，一条命令都不发
	// （否则会出现"新手链那组已经发出去了、抓鬼这组失败"的半成功状态）。
	var ghostNav *chainlib.Chain
	if len(toGhost) > 0 {
		nav, err := a.chainPayloads().Ghost()
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "mode": "auto", "sent": 0, "accounts": len(targets),
				"groups":      map[string]any{"start_chain": []string{}, "ghost_start": toGhost},
				"assignments": assignments, "chain_id": defaultChainID,
				"msg": "抓鬼导航数据不可用，未下发任何命令：" + err.Error() +
					"（把导航数据放进链目录，或用 CTRL_GHOST_NAV_CHAIN 指定别的文件名）",
			})
			return
		}
		ghostNav = nav
	}

	sent, msgs := 0, []string{}
	if len(toChain) > 0 {
		cmd := map[string]any{"cmd": "start_chain", "chain_id": defaultChainID}
		switch {
		case inlineChain != nil:
			cmd["chain"] = inlineChain
		default:
			if chain, err := chainlib.Build(defaultChainID, a.Cfg.ChainDir); err == nil {
				cmd["chain"] = chain
				if chain.ChainID != "" {
					defaultChainID = chain.ChainID
					cmd["chain_id"] = defaultChainID
				}
			} else if !errors.Is(err, chainlib.ErrNotFound) {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "链数据解析失败: " + err.Error()})
				return
			}
		}
		for _, as := range assignments { // 回填真实链 id（文件里声明的优先）
			if as["command"] == "start_chain" {
				as["chain_id"] = cmd["chain_id"]
			}
		}
		cmd["accounts"] = toChain
		if a.Events.SendCmd(cmd, "start_chain_auto") {
			sent++
			a.markChainDispatch(toChain) // 2026-09-23 R1：新手链在途记账（配额/候选/去重据此）
			msgs = append(msgs, fmt.Sprintf("%d 个走新手链(%s)", len(toChain), defaultChainID))
		}
	}
	if len(toGhost) > 0 {
		// 抓鬼必须带导航数据：机器人端只认 cmd["chain"]（npcs/map_grids/dijkstra/ghost_maps/
		// ghost_map_pos 全在里面），不给就是"原地不动"。role 显式带 solo（与机器人默认一致）。
		ghostChainID := a.chainPayloads().GhostNavChainID()
		if ghostNav != nil && ghostNav.ChainID != "" {
			ghostChainID = ghostNav.ChainID
		}
		cmd := map[string]any{
			"cmd": "ghost_start", "chain_id": ghostChainID, "chain": ghostNav,
			"role": "solo", "daily_limit": a.chainPayloads().GhostDailyLimit(),
			"accounts": toGhost,
		}
		if done := a.ghostDoneMap(toGhost); len(done) > 0 {
			cmd["done"] = done // 重新下发不丢进度（参考实现 routers/ghost.py 同款）
		}
		if a.Events.SendCmd(cmd, "ghost_start_auto") {
			sent++
			a.markGhostDispatch(toGhost) // 2026-09-23 P0：在途记账（配额/池闸据此防连续放行）
			msgs = append(msgs, fmt.Sprintf("%d 个走钟馗抓鬼(%s)", len(toGhost), ghostChainID))
		}
	}

	if len(skipped) > 0 {
		msgs = append(msgs, fmt.Sprintf("跳过 %d 个（等级不够/不派抓鬼）", len(skipped)))
	}
	msg := "按意图自动分配：没有需要启动的账号"
	if len(msgs) > 0 {
		msg = "按意图自动分配：" + strings.Join(msgs, "；")
	}
	ok := sent > 0
	a.Store.LogEvent(map[string]any{"type": "api", "action": "start_auto", "zone": a.currentZoneKey(),
		"accounts": targets, "sent": sent, "assignments": assignments})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "mode": "auto", "sent": sent, "accounts": len(targets),
		"groups":      map[string]any{"start_chain": toChain, "ghost_start": toGhost},
		"assignments": assignments, "chain_id": defaultChainID, "skipped": skipped,
		"msg": okMsg(ok, msg),
	})
}

// intentKinds 运行时意图表快照 → (kind, reason) 两个索引（按账号）。
func (a *API) intentKinds() (map[string]intent.Kind, map[string]string) {
	kinds := map[string]intent.Kind{}
	reasons := map[string]string{}
	if a.Events != nil && a.Events.Intents != nil {
		for _, it := range a.Events.Intents.Snapshot() {
			kinds[it.Account] = it.Kind
			reasons[it.Account] = it.Reason
		}
	}
	return kinds, reasons
}

// decideKind 该账号"应该跑哪条链"：优先运行时意图表，其次账号池里的当前条件。
//
// 池内回退的由来（判据与「自动分配」完全一致，勿改）：
// ≥31 级或链已完成 → 抓鬼；<31 → 新手链；等级未知 → 不瞎判（返回空 kind，由调用方回落指定链）。
func (a *API) decideKind(acc string, kinds map[string]intent.Kind, reasons map[string]string) (intent.Kind, string) {
	if kind := kinds[acc]; kind != "" {
		return kind, reasons[acc]
	}
	if a.Accounts == nil || a.Events == nil || a.Events.Intents == nil {
		return "", ""
	}
	pa, ok := a.Accounts.Get(acc)
	if !ok {
		return "", ""
	}
	cur := a.gameAddrOf("")
	lv, done := 0, pa.ChainDone
	if z := pa.Zone(cur); z != nil {
		lv, done = z.Level, done || z.ChainDone
	} else { // 没有当前区记录：取任意一个（按区键排序，保证确定性）
		keys := make([]string, 0, len(pa.Zones))
		for k := range pa.Zones {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if z := pa.Zones[k]; z != nil {
				lv, done = z.Level, done || z.ChainDone
				break
			}
		}
	}
	d := a.Events.Intents.Decider().Decide(lv, done)
	if !d.Known {
		return "", "" // 等级未知：不瞎判，回落请求给的链
	}
	return d.Kind, d.Reason + "（按池内记录）"
}

func (a *API) handleStop(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	accounts := bodyAccounts(body)
	// 停链同时**取消自动重登恢复**：用户手动停了就不该被自动拉起
	//（口径同参考实现 routers/ghost.py 的 _PENDING_REGHOST.pop）。
	a.cancelRegHost(accounts)
	// 2026-09-23 R3：停止 = **人工暂停** —— 给这些号打暂停标，让恢复引擎/定时任务/
	// 水位/游荡池派发前跳过（生产 CTRL_AUTO_RESTORE=1，不打标会在几秒内被补发拉起，
	// 用户观感"停不住"）。解除方式：再次「启动 / 立即补发 / 上线」（见 pause.go）。
	pausedN := a.pauseAccounts(accounts)
	cmd := map[string]any{"cmd": "stop"}
	if len(accounts) > 0 {
		cmd["accounts"] = accounts
	}
	ok := a.Events.SendCmd(cmd, "stop")
	a.Store.LogEvent(map[string]any{"type": "api", "action": "stop",
		"zone": a.currentZoneKey(), "accounts": accounts, "paused": pausedN, "sent": ok})
	msg := okMsg(ok, "已下发停链")
	msg += "；已标人工暂停 " + itoa(pausedN) + " 个（自动编排不再拉起；再点「启动/立即补发/上线」解除）"
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "paused": pausedN, "msg": msg})
}

// cancelRegHost 取消自动重登恢复：给了账号就取消这些；没给（=全部）就清空待恢复列表。
func (a *API) cancelRegHost(accounts []string) {
	if a.Reghost == nil {
		return
	}
	if len(accounts) == 0 {
		for _, st := range a.Reghost.Status() {
			a.Reghost.Cancel(st.Account)
		}
		return
	}
	for _, acc := range accounts {
		a.Reghost.Cancel(acc)
	}
}

func (a *API) handleReset(w http.ResponseWriter, r *http.Request) {
	a.simpleCommand(w, r, "reset", "已下发重置重跑")
}

// simpleCommand stop/reset 共用。
func (a *API) simpleCommand(w http.ResponseWriter, r *http.Request, cmdName, okText string) {
	body := readBody(r)
	accounts := bodyAccounts(body)
	cmd := map[string]any{"cmd": cmdName}
	if len(accounts) > 0 {
		cmd["accounts"] = accounts
	}
	ok := a.Events.SendCmd(cmd, cmdName)
	a.Store.LogEvent(map[string]any{"type": "api", "action": cmdName,
		"zone": a.currentZoneKey(), "accounts": accounts, "sent": ok})
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": okMsg(ok, okText)})
}

// handleRobotsManage 动态添加/移除机器人。
func (a *API) handleRobotsManage(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	action := toStr(body["action"])
	pairs := a.accountPairs(body)
	if len(pairs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "accounts 为空"})
		return
	}
	// 密码：请求里给的 → 账号库里该区的。都没有就报出来（**没有统一密码**）
	zone := a.gameAddrOf(zoneKeyOf(r, body))
	noPwd := make([]string, 0)
	for i := range pairs {
		if pairs[i][1] == "" {
			if pwd, ok := a.Accounts.PasswordFor(pairs[i][0], zone); ok {
				pairs[i][1] = pwd
			} else {
				noPwd = append(noPwd, pairs[i][0])
			}
		}
	}
	switch action {
	case "add":
		if len(noPwd) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false,
				"msg": "这些账号库里没有密码（先建号/补密码；本项目没有统一密码）：" + strings.Join(noPwd, ", ")})
			return
		}
		payload := make([]any, 0, len(pairs))
		names := make([]string, 0, len(pairs))
		for _, p := range pairs {
			payload = append(payload, []string{p[0], p[1]})
			names = append(names, p[0])
		}
		ok := a.Events.SendCmd(map[string]any{
			"cmd": "robot_manage", "action": "add", "accounts": payload}, "add")
		if ok {
			// 2026-09-23 R3：显式添加=上线 → 解除人工暂停（用户让它干活）。
			a.resumeAccounts(names)
		}
		a.Store.LogEvent(map[string]any{"type": "api", "action": "robots_manage", "sub": "add",
			"zone": a.currentZoneKey(), "accounts": names, "sent": ok})
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": okMsg(ok, "已下发添加机器人")})
	case "remove":
		names := make([]string, 0, len(pairs))
		for _, p := range pairs {
			names = append(names, p[0])
			a.St.MarkRemoved(p[0]) // 防心跳复活
			a.St.Remove(p[0])
			a.cancelRegHost([]string{p[0]}) // 手动移除 → 取消自动重登恢复
		}
		ok := a.Events.SendCmd(map[string]any{
			"cmd": "robot_manage", "action": "remove", "accounts": names}, "remove")
		a.Store.LogEvent(map[string]any{"type": "api", "action": "robots_manage", "sub": "remove",
			"accounts": names, "sent": ok})
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": okMsg(ok, "已下发移除机器人")})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "action 必须是 add/remove"})
	}
}

// handleRobotRestart 重启机器人进程（单进程）。
func (a *API) handleRobotRestart(w http.ResponseWriter, r *http.Request) {
	if err := a.Proc.Restart(a.Cfg.KillRobots); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "robot_restart",
		"zone": a.currentZoneKey(), "deploy_dir": a.Cfg.DeployDir, "sent": true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"msg": "机器人进程已重启（部署目录 " + a.Cfg.DeployDir + "）"})
}

// handleRobotsClearRemoved 清空"已移除"名单（POST /api/robots/clear_removed）。
//
// 用途（2026-09-22 生产）：AutoRemoveOnDone 会把某些原因下线的号永久排除出候选池，
// 池子被吃空后水位器补不到号（"号池里没有可上线的号"）、在线数上不去。
// 这里一键清空名单（不重启、不断控制通道）→ 候选池立即恢复，随后由水位器/各池
// 在下一轮（≤60s）按需补号。注意：被清掉的号若"今日满额"仍会被 MarkGhostDoneToday
// 挡住（当天不派），属预期行为。
func (a *API) handleRobotsClearRemoved(w http.ResponseWriter, r *http.Request) {
	if a.St == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "state 不可用"})
		return
	}
	names := a.St.ClearRemoved()
	a.Store.LogEvent(map[string]any{"type": "api", "action": "robots_clear_removed",
		"zone": a.currentZoneKey(), "count": len(names)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(names),
		"msg": fmt.Sprintf("已清空移除名单（%d 个号回到候选池，水位器/池子将在下一轮按需补号）", len(names))})
}

// handleReloadScripts 热更机器人脚本（进程内 importlib.reload 指定模块，免重启）。
//
//	POST /api/robot/reload_scripts
//	{"modules": ["quest_engine"]}   // 省略 = 机器人端默认（quest_engine）
//
// 机器人端说明（client.py reload_scripts 分支）：reload 会重置模块级缓存
// （导航链/网格缓存，下次收到带 chain 的命令会重新填充），建议任务间隙触发；
// 已实例化的 QuestState 保留旧类，新属性访问须 getattr 兜底。
// scriptsNoReload 协议/框架层模块（禁热更；改动走重启机器人）。
//
// 2026-09-23 生产实证：reload protocol3/msghandle 在多线程收包环境下触发
// 0xc0000005 访问冲突 → 机器人进程崩溃、全部号掉线。这类模块只能冷启动加载。
var scriptsNoReload = map[string]bool{
	"protocol3": true, "msghandle": true, "cnet": true, "protocol": true,
	"cnetwork": true, "robot_mgr": true, "robot": true, "client": true,
	"main_tester": true,
}

func (a *API) handleReloadScripts(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	mods := make([]string, 0, 2)
	if arr, ok := body["modules"].([]any); ok {
		for _, m := range arr {
			if s := strings.TrimSpace(toStr(m)); s != "" {
				mods = append(mods, s)
			}
		}
	}
	// 2026-09-23 协议/框架层模块禁热更（生产实证：reload protocol3/msghandle 令机器人
	// 访问冲突崩溃）—— 在下发前过滤，返回被拦清单（这类改动必须重启机器人）。
	blocked := make([]string, 0, 2)
	kept := make([]string, 0, len(mods))
	for _, m := range mods {
		if scriptsNoReload[m] {
			blocked = append(blocked, m)
			continue
		}
		kept = append(kept, m)
	}
	mods = kept
	cmd := map[string]any{"cmd": "reload_scripts"}
	if len(mods) > 0 {
		cmd["modules"] = mods
	}
	ok := a.Events != nil && a.Events.SendCmd(cmd, "reload_scripts")
	a.Store.LogEvent(map[string]any{"type": "api", "action": "reload_scripts",
		"zone": a.currentZoneKey(), "modules": mods, "sent": ok})
	msg := okMsg(ok, "已下发脚本热更")
	if ok && len(mods) > 0 {
		msg += "（" + strings.Join(mods, ",") + "）"
	}
	if len(blocked) > 0 {
		msg += "；已拦截（协议/框架层需重启机器人）: " + strings.Join(blocked, ",")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "modules": mods,
		"blocked": blocked, "msg": msg})
}

// ---------------------------------------------------------------- WebSocket

func (a *API) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsutil.Upgrade(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	client := a.WS.addClient(conn)
	defer a.WS.removeClient(client)
	// 读取循环：客户端消息不消费（只用于探测断开）；ping 由 wsutil 内部回 pong。
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------- 小工具

// splitZoneKey 把 "<服key>/<区key>" 拆开（缺省返回空串）。
func splitZoneKey(key string) (serverKey, zoneKey string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
}
