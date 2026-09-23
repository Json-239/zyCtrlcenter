package api

import (
	"net/http"
	"strings"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
)

// ---------------------------------------------------------------- 账号池

// handleAccountsList 账号池列表（池数据 + 运行时状态合并；**不下发密码**）。
//
//	GET /api/accounts?zone=47.96.8.240:2300&usable=1&keyword=robot0001&limit=100&offset=0&online=1
func (a *API) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	zone := strings.TrimSpace(q.Get("zone"))
	usable := q.Get("usable") == "1" || q.Get("usable") == "true"
	onlyOnline := q.Get("online") == "1" || q.Get("online") == "true"
	limit := toInt(q.Get("limit"), 200)
	offset := toInt(q.Get("offset"), 0)
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	poolArg := strings.ToLower(strings.TrimSpace(q.Get("pool"))) // newbie | ghost（号池分区筛选）
	all := a.Accounts.List(accounts.Filter{
		Keyword: q.Get("keyword"), Zone: zone, Usable: usable,
	})
	// 号池分区（新手池/抓鬼池）在 api 侧按"等级+毕业状态"划分，所以要**先分区再分页**，
	// 否则筛出来的一页会被池内其它号挤掉。
	split := map[string]int{"newbie": 0, "ghost": 0, "unknown": 0}
	pool := make([]*accounts.Account, 0, len(all))
	for _, acc := range all {
		k := a.poolOf(acc.Name)
		split[k]++
		if poolArg == "" || poolArg == k {
			pool = append(pool, acc)
		}
	}
	if offset > 0 {
		if offset >= len(pool) {
			pool = nil
		} else {
			pool = pool[offset:]
		}
	}
	if limit > 0 && len(pool) > limit {
		pool = pool[:limit]
	}

	rows := make([]map[string]any, 0, len(pool))
	for _, acc := range pool {
		row := map[string]any{
			"name": acc.Name, "has_password": acc.Password != "",
			"pool": a.poolOf(acc.Name), // 号池分区：newbie 新手池 / ghost 抓鬼池 / unknown
			"level": acc.Level, "role_name": acc.RoleName, "chain_done": acc.ChainDone,
			"note": acc.Note, "task_type": acc.TaskType, "last_online": acc.LastOnline,
			"usable": acc.UsableIn(zone), "online": false,
		}
		// 指定区（或"任意可用区"）的状态
		z := acc.Zones[zone]
		if zone == "" { // 未指定区：取第一个有状态的区做展示
			for k, v := range acc.Zones {
				if v != nil && (z == nil || v.Usable) {
					z, row["zone"] = v, k
					if v.Usable {
						break
					}
				}
			}
		}
		if z != nil {
			row["verified"] = z.Verified
			row["usable"] = z.Usable
			row["verify_msg"] = z.Msg
			row["verified_at"] = z.VerifiedAt // 最近一次可用性验证时间（0=从未验过）
			if z.Level > 0 {
				row["level"] = z.Level
			}
			if z.RoleName != "" {
				row["role_name"] = z.RoleName
			}
		}
		// 运行时状态（在线/等级/角色取最新）
		if live, ok := a.St.Get(acc.Name); ok {
			row["online"] = live.Online
			row["state"] = live.State
			row["runtime_zone"] = live.Zone
			row["mapid"] = live.MapID
			row["pos_grid"] = live.PosGrid
			if live.Level > 0 {
				row["level"] = live.Level
			}
			if live.RoleName != "" {
				row["role_name"] = live.RoleName
			}
			row["chain_done"] = live.ChainDone || acc.ChainDone
		}
		if onlyOnline && row["online"] != true {
			continue
		}
		rows = append(rows, row)
	}

	// 池里没有、但当前在线的号也补进来（手工 add 过、未导入池）
	if q.Get("include_live") != "0" {
		seen := map[string]bool{}
		for _, acc := range pool {
			seen[acc.Name] = true
		}
		for _, live := range a.St.Snapshot() {
			if seen[live.Account] || (onlyOnline && !live.Online) {
				continue
			}
			livePool := a.poolOf(live.Account)
			if poolArg != "" && poolArg != livePool { // 号池分区筛选：池外在线行也要跟着筛
				continue
			}
			if kw := strings.ToLower(strings.TrimSpace(q.Get("keyword"))); kw != "" &&
				!strings.Contains(strings.ToLower(live.Account), kw) {
				continue
			}
			rows = append(rows, map[string]any{
				"name": live.Account, "in_pool": false, "online": live.Online, "pool": livePool,
				"state": live.State, "level": live.Level, "role_name": live.RoleName,
				"chain_done": live.ChainDone, "runtime_zone": live.Zone,
				"mapid": live.MapID, "pos_grid": live.PosGrid,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(rows), "pool": a.Accounts.Count(),
		"pool_filter": poolArg, "pool_split": split, // 号池分区：筛选口径 + 三池数量
		"stats": a.Accounts.Stats(zone), "zone": zone,
		"meta": a.Accounts.Meta(), "accounts": rows,
	})
}

// handleAccountsStats 账号池统计（可按区）。
func (a *API) handleAccountsStats(w http.ResponseWriter, r *http.Request) {
	zone := strings.TrimSpace(r.URL.Query().Get("zone"))
	online, _, total := a.St.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "zone": zone, "stats": a.Accounts.Stats(zone),
		// pending：该区还有多少待验证/已知不可用（面板"批量验证"显示"本次将验 N 个"）
		"pending":      a.Accounts.CountForVerify(zone),
		"online_total": online, "robot_total": total, "meta": a.Accounts.Meta(),
	})
}

// handleAccountsAdd 加号入池（可顺带分配给区）。
func (a *API) handleAccountsAdd(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	names := bodyAccounts(body)
	if len(names) == 0 { // 也支持 "names": "a,b,c"
		names = splitList(toStr(body["names"]))
	}
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "accounts/names 为空"})
		return
	}
	pwd := toStr(body["password"])
	zone := toStr(body["zone"])
	note := toStr(body["note"])
	added, existed := a.Accounts.Add(names, pwd, zone, note)
	a.Store.LogEvent(map[string]any{"type": "api", "action": "accounts_add",
		"accounts": names, "added": len(added), "existed": len(existed), "zone": zone})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": added, "existed": existed, "pool": a.Accounts.Count(),
		"msg": "已加池 " + itoa(len(added)) + " 个（已存在 " + itoa(len(existed)) + " 个）",
	})
}

// handleAccountsRemove 从池里删号（不动游戏服上的号）。
func (a *API) handleAccountsRemove(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	names := bodyAccounts(body)
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "accounts 为空"})
		return
	}
	n := a.Accounts.Remove(names)
	a.Store.LogEvent(map[string]any{"type": "api", "action": "accounts_remove",
		"accounts": names, "removed": n})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n,
		"msg": "已从池里删除 " + itoa(n) + " 个（游戏服上的账号不受影响）"})
}

// ---------------------------------------------------------------- 批量上下线

// handleRobotsBatch 批量上下线机器人。
//
//	POST /api/robots/batch
//	{ "action": "online"|"offline",
//	  "accounts": [...]?,          // 省略 = 从池里按区选（online）/ 取当前在线（offline）
//	  "zone": "47.96.8.240:2300"?, // 选号时按该区可用性过滤（默认当前区的游戏服地址）
//	  "limit": 20,                 // online 选号上限（默认 20，硬上限 500）
//	  "chunk": 10,                 // 每条命令带多少个账号（默认 10）
//	  "interval_ms": 500,          // 批间隔（默认 500ms，避免登录风暴）
//	  "only_usable": true,         // 只选可用号（默认 true）
//	  "force": false }             // online：跳过池容量闸（默认 false，见 batchOnlineBudgetOf）
func (a *API) handleRobotsBatch(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	action := strings.ToLower(toStr(body["action"]))
	accounts := normAccounts(bodyAccounts(body)) // 勾选集合：去空白/空串/重复
	zoneArg := toStr(body["zone"])
	gameAddr := a.gameAddrOf(zoneArg) // 池里用的是 "host:port"（游戏服地址）
	limit := toInt(body["limit"], 20)
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	chunk := toInt(body["chunk"], 10)
	if chunk <= 0 || chunk > 50 {
		chunk = 10
	}
	interval := toInt(body["interval_ms"], 500)
	if interval < 0 || interval > 60000 {
		interval = 500
	}
	onlyUsable := toBool(body["only_usable"], true)
	force := toBool(body["force"], false)

	switch action {
	case "online":
		a.batchOnline(w, accounts, gameAddr, zoneArg, limit, chunk, interval, onlyUsable, force)
	case "offline":
		a.batchOffline(w, accounts, gameAddr, chunk, interval)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "action 必须是 online/offline"})
	}
}

// batchOnline 批量上线：选号 → 池容量闸 → 分批下发 robot_manage add。
//
// 2026-09-23 P2（三池对齐）：新增**池容量闸** —— 批量上线原先没有任何池/容量约束，
// 一次可 add 100~200 个号；这些号上线后被机器人端 auto_roam 兜底游荡、或按意图被各池
// 补号通道拉去干活，把三池目标（抓鬼/新手/游荡余量）冲垮（见
// docs/04-测试/分析-20260923-抓鬼分配逻辑.md 第四节"无约束批量上线"）。口径与逃生通道
// 见 batchOnlineBudgetOf；超出容量的号本次不上线（不"上线后再移出"——团队口径明确否掉
// "下线待命"：水位器会把压掉的号补回来，形成往返）。
func (a *API) batchOnline(w http.ResponseWriter, accounts []string, gameAddr, zoneArg string,
	limit, chunk, interval int, onlyUsable, force bool) {

	online := map[string]bool{}
	for _, live := range a.St.Snapshot() {
		if live.Online {
			online[live.Account] = true
		}
	}
	skipped := []string{}
	if len(accounts) == 0 { // 自动选号
		accounts = a.Accounts.Pick(gameAddr, limit, onlyUsable, online)
	} else { // 显式给号：过滤掉已在线
		kept := make([]string, 0, len(accounts))
		for _, n := range accounts {
			if online[n] {
				skipped = append(skipped, n)
				continue
			}
			kept = append(kept, n)
		}
		accounts = kept
	}
	// 2026-09-23 P2：池容量闸 —— 超出"三池可承载"的号本次不上线（force 跳过）。
	budget := a.batchOnlineBudgetOf()
	queued := []string{}
	if !force && budget.cap > 0 && len(accounts) > budget.allow {
		queued = append(queued, accounts[budget.allow:]...)
		accounts = accounts[:budget.allow]
		a.Log.Printf("[BATCH] 池容量闸拦下 %d 个（容量 %d/%s，已占 %d，额度 %d）：%s",
			len(queued), budget.cap, budget.source, budget.online, budget.allow, strings.Join(queued, ", "))
	}

	if len(accounts) == 0 {
		if len(queued) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "requested": 0, "sent": 0, "accounts": []string{},
				"queued": queued, "queued_count": len(queued), "forced": force,
				"pool_cap": budget.cap, "pool_online": budget.online,
				"pool_allow": budget.allow, "cap_source": budget.source,
				"skipped_online": skipped,
				"msg": "池容量已满（" + itoa(budget.online) + "/" + itoa(budget.cap) + "，" + budget.source + "）：" +
					itoa(len(queued)) + " 个号本次未上线；等池内收工或调大目标后再上，或传 force:true 强制",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "skipped_online": skipped,
			"msg": "没有可上线的账号（都已在线或池里没有可用号）"})
		return
	}

	sentAccounts, chunks, skippedNoPwd := a.sendOnlineChunks(accounts, gameAddr, chunk, interval, "batch_add")
	sent := len(sentAccounts)
	a.onlineInflight.mark(sentAccounts) // P2：上线在途记账（容量闸防"上一批还在登录路上又放行下一批"）
	// 2026-09-23 R3：显式上线 = 用户让它干活 → 解除人工暂停（只解除本次真的下发成功的号）。
	a.resumeAccounts(sentAccounts)
	a.Store.LogEvent(map[string]any{"type": "api", "action": "robots_batch", "sub": "online",
		"zone": zoneArg, "requested": len(accounts), "sent": sent, "chunks": chunks,
		"interval_ms": interval, "queued": queued, "force": force,
		"pool_cap": budget.cap, "pool_online": budget.online, "cap_source": budget.source})
	msg := "已分 " + itoa(chunks) + " 批下发上线（" + itoa(sent) + "/" + itoa(len(accounts)) + " 个）"
	if len(queued) > 0 {
		msg += "；" + itoa(len(queued)) + " 个未上线（池容量已满 " + itoa(budget.online) + "/" +
			itoa(budget.cap) + "；等池内收工或调大目标，传 force:true 可强制）"
	}
	if len(skippedNoPwd) > 0 {
		msg += "；跳过 " + itoa(len(skippedNoPwd)) + " 个库里没密码的号"
	}
	if sent == 0 {
		msg = "下发失败：机器人通道未连接"
		if len(skippedNoPwd) > 0 {
			msg = "没有可下发的账号：这些号在库里没有密码"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": sent > 0, "msg": msg, "requested": len(accounts), "sent": sent,
		"chunks": chunks, "accounts": accounts, "skipped_online": skipped,
		"skipped_no_password": skippedNoPwd, "queued": queued, "queued_count": len(queued),
		"forced": force, "pool_cap": budget.cap, "pool_online": budget.online,
		"pool_allow": budget.allow, "cap_source": budget.source,
		"zone":                zoneArg, "game_addr": gameAddr,
	})
}

// ---------------------------------------------------------------- 批量上线池容量闸（2026-09-23 P2）

// batchOnlineBudget 批量上线的"池容量预算"读数（见 batchOnlineBudgetOf）。
type batchOnlineBudget struct {
	cap    int    // 容量（0 = 三池都没配目标 → 不约束，保持旧行为）
	source string // 容量来源："waterline"（水位目标）/ "pools"（三池目标之和）/ "none"
	online int    // 已占：当前区在线数 + 已下发还没上线的在途数
	allow  int    // 本次还能上线的额度 = max(0, cap - online)
}

// batchOnlineBudgetOf 计算批量上线的池容量预算（纯读，不动状态）。
//
// 背景（docs/04-测试/分析-20260923-抓鬼分配逻辑.md）：面板「批量上线」原先无任何池/容量约束，
// 一次可 add 100~200 个号；上线后被机器人端 auto_roam 兜底游荡、或按意图被各池补号通道拉去
// 干活，把三池目标（抓鬼/新手/游荡余量）冲垮 —— 与 /api/start(auto)、RESTORE 补发并列为超编
// 来源。这里给批量上线补上"总容量"闸：
//
//	容量 cap  = 水位 target（水位**启用且配了目标**时；用户口径的"在线总数"就是这个数）
//	          = 否则 Σ 三池 target（抓鬼 + 新手 + 游荡里 >0 的；水位没开时的回退口径）
//	          = 都没配 → 0 → **不约束**（保持旧行为：未配置环境/测试不受影响）
//	已占 online = 当前区在线号数 + onlineInflight 在途数（已下发 add、还没上线的号；
//	              防"上一批还在登录路上，下一批又按旧在线数放行"的连批叠加）
//	额度 allow = max(0, cap - online)
//
// 为什么用"总量"尺度而不是"按池归口"：号上线后去哪由池的**运行期判决**决定（抓鬼满额转
// 游荡、任务池缺人回收游荡号、满额转游荡），按池硬分会让"该去游荡的满额号"上不了线。
//
// 逃生通道：请求带 force=true 跳过本闸（临时加压/排障）；被拦下的号作为 queued 回带，
// **不**做"上线后再移出"（团队口径明确否掉"下线待命"：水位器会把压掉的号补回来，往返空转）。
func (a *API) batchOnlineBudgetOf() batchOnlineBudget {
	b := batchOnlineBudget{source: "none"}
	// ① 容量：水位目标优先（它就是"在线总数"的口径），否则回退三池目标之和。
	if a.Waterline != nil {
		if st := a.Waterline.Status(); st.Enabled && st.Target > 0 {
			b.cap, b.source = st.Target, "waterline"
		}
	}
	if b.cap <= 0 {
		if a.AutoTask != nil {
			states := a.AutoTask.States()
			for _, k := range []autotask.Kind{autotask.KindGhost, autotask.KindNewbie} {
				if st, ok := states[k]; ok && st.Target > 0 {
					b.cap += st.Target
				}
			}
		}
		if a.Roampool != nil {
			if t := a.Roampool.Config().Target; t > 0 {
				b.cap += t
			}
		}
		if b.cap > 0 {
			b.source = "pools"
		}
	}
	// ② 已占：当前区在线数（本地快照；别的区的号不占本区额度）。
	if a.St != nil {
		key := a.currentZoneKey()
		for _, r := range a.St.Snapshot() {
			if !r.Online {
				continue
			}
			if key != "" && r.Zone != "" && r.Zone != key {
				continue
			}
			b.online++
		}
	}
	// ③ 已占：上线在途（刚下发 add、还没上线；已上线的号按实时状态从在途表剔除）。
	b.online += a.onlineInflight.count(func(acc string) bool {
		if a.St == nil {
			return false
		}
		r, ok := a.St.Get(acc)
		return ok && r.Online // 命令已生效（号已上线）→ 交给在线数统计，不重复占用
	})
	b.allow = b.cap - b.online
	if b.allow < 0 {
		b.allow = 0
	}
	return b
}

// sendOnlineChunks 分批下发 robot_manage add（**批量上线/定时任务/水位保持共用这一条通路**）。
//
// 密码只从账号库按区取（没有统一密码）；库里没密码的号跳过并记进 noPwd。
// 返回：sent = 实际进载荷并成功下发的账号（顺序同入参）、chunks = 下发批数、noPwd = 被跳过的号。
func (a *API) sendOnlineChunks(accounts []string, gameAddr string, chunk, interval int, tag string) (sent []string, chunks int, noPwd []string) {
	if chunk <= 0 {
		chunk = 10
	}
	sent = make([]string, 0, len(accounts))
	for start := 0; start < len(accounts); start += chunk {
		end := start + chunk
		if end > len(accounts) {
			end = len(accounts)
		}
		batch := accounts[start:end]
		payload := make([]any, 0, len(batch))
		for _, n := range batch {
			pwd, ok := a.Accounts.PasswordFor(n, gameAddr)
			if !ok {
				noPwd = append(noPwd, n)
				continue
			}
			payload = append(payload, []string{n, pwd})
		}
		if len(payload) == 0 {
			continue
		}
		if a.Events.SendCmd(map[string]any{
			"cmd": "robot_manage", "action": "add", "accounts": payload}, tag) {
			sent = append(sent, batch...)
		}
		chunks++
		if end < len(accounts) && interval > 0 {
			time.Sleep(time.Duration(interval) * time.Millisecond)
		}
	}
	// 2026-09-23 选号轮换：把"刚下发上线"记进账号池的 last_online（三个入口共用本通路：
	// 批量上线 / 定时任务 / 水位补号）——选号（accounts.Pick / waterline 补号）按
	// "最久未上线优先"挑号，靠这里把刚用过的号排到后面，轮换才会真正轮起来。
	if len(sent) > 0 && a.Accounts != nil {
		a.Accounts.TouchOnline(sent, time.Now().Unix())
	}
	return sent, chunks, noPwd
}

// batchOffline 批量下线：robot_manage remove + 本地标记移除（防心跳复活）。
func (a *API) batchOffline(w http.ResponseWriter, accounts []string, gameAddr string, chunk, interval int) {
	if len(accounts) == 0 {
		for _, live := range a.St.Snapshot() {
			if live.Online {
				accounts = append(accounts, live.Account)
			}
		}
	}
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "没有在线账号可下线"})
		return
	}
	sentAccounts, chunks := a.sendOfflineChunks(accounts, chunk, interval, "batch_remove")
	sent := len(sentAccounts)
	a.Store.LogEvent(map[string]any{"type": "api", "action": "robots_batch", "sub": "offline",
		"zone": gameAddr, "requested": len(accounts), "sent": sent, "chunks": chunks})
	msg := "已分 " + itoa(chunks) + " 批下发下线（" + itoa(sent) + "/" + itoa(len(accounts)) + " 个）"
	if sent == 0 {
		msg = "下发失败：机器人通道未连接（本地已标记移除）"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": sent > 0, "msg": msg, "requested": len(accounts), "sent": sent,
		"chunks": chunks, "accounts": accounts,
	})
}

// sendOfflineChunks 分批下发 robot_manage remove（**批量下线/水位保持共用这一条通路**）。
//
// 先本地标记移除 + 删行（防心跳复活），再分批下发（每批之间留 interval，避免下线风暴）。
// 返回实际下发成功的账号与批数。
//
// 2026-09-23 R4（操作健壮性审计）：这里统一**取消自动重登恢复（reghost）** ——
// 否则"当日卡死过"的号刚被下线就被 reghost 重登 + 补发（用户观感"下线没效果"）。
// 口径与 /api/robots/manage remove（handlers.go）和 /api/stop 一致；Cancel 幂等。
func (a *API) sendOfflineChunks(accounts []string, chunk, interval int, tag string) (sent []string, chunks int) {
	if chunk <= 0 {
		chunk = 10
	}
	a.cancelRegHost(accounts)
	for _, n := range accounts {
		a.St.MarkRemoved(n)
		a.St.Remove(n)
	}
	sent = make([]string, 0, len(accounts))
	for start := 0; start < len(accounts); start += chunk {
		end := start + chunk
		if end > len(accounts) {
			end = len(accounts)
		}
		batch := accounts[start:end]
		if a.Events.SendCmd(map[string]any{
			"cmd": "robot_manage", "action": "remove", "accounts": batch}, tag) {
			sent = append(sent, batch...)
		}
		chunks++
		if end < len(accounts) && interval > 0 {
			time.Sleep(time.Duration(interval) * time.Millisecond)
		}
	}
	return sent, chunks
}

// gameAddrOf 把区 key（"服/区"）换算成池里用的游戏服地址（"host:port"）。
// 传空时用当前区的地址；传的已经是 "host:port" 时原样返回。
// poolOf 号池分区（从本地库 + 运行时状态判据划出来，不额外落盘）：
//
	//	newbie —— 新手池：未毕业（等级 <31 且未 chain_done）
	//	ghost  —— 抓鬼池：已毕业（≥31 级 或 chain_done）
	//	unknown—— 等级未知且无记录（先把号上线跑一次，机器人报等级后自动归位）
//
// 口径与意图判定（internal/services/intent）同源：**只按等级/毕业状态**，不看"能不能派出去"。
func (a *API) poolOf(acc string) string {
	level, _ := a.accountLevel(acc)
	chainDone := false
	if a.St != nil {
		if r, ok := a.St.Get(acc); ok {
			chainDone = r.ChainDone
		}
	}
	if a.Accounts != nil {
		if pa, ok := a.Accounts.Get(acc); ok {
			chainDone = chainDone || pa.ChainDone
			if level == 0 {
				if z := pa.Zone(a.gameAddrOf("")); z != nil {
					level = z.Level
				}
			}
		}
	}
	if level <= 0 && !chainDone {
		// 等级还没上报：**已在该区验证可用**的号按新手池算（全新号天然该跑新手链；
		// 上线跑一次机器人就会报等级，之后按真实等级归位）。验证都没过的才算未知。
		if a.Accounts != nil && a.Accounts.Usable(acc, a.gameAddrOf("")) {
			return "newbie"
		}
		return "unknown"
	}
	if chainDone || level >= a.newbieMaxLevel() {
		return "ghost"
	}
	return "newbie"
}
func (a *API) gameAddrOf(zoneKey string) string {
	if strings.Contains(zoneKey, ":") {
		return zoneKey
	}
	if zoneKey == "" {
		if cur, ok := a.Zones.Current(); ok {
			return cur.Addr()
		}
		return ""
	}
	if flat, ok := a.Zones.FlatOf(splitZoneKey(zoneKey)); ok {
		return flat.Addr()
	}
	return zoneKey
}
