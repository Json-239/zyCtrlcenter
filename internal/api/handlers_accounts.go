package api

import (
	"net/http"
	"strings"
	"time"

	"zyctrlcenter/internal/services/accounts"
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
//	  "only_usable": true }        // 只选可用号（默认 true）
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

	switch action {
	case "online":
		a.batchOnline(w, accounts, gameAddr, zoneArg, limit, chunk, interval, onlyUsable)
	case "offline":
		a.batchOffline(w, accounts, gameAddr, chunk, interval)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "action 必须是 online/offline"})
	}
}

// batchOnline 批量上线：选号 → 分批下发 robot_manage add。
func (a *API) batchOnline(w http.ResponseWriter, accounts []string, gameAddr, zoneArg string,
	limit, chunk, interval int, onlyUsable bool) {

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
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "skipped_online": skipped,
			"msg": "没有可上线的账号（都已在线或池里没有可用号）"})
		return
	}

	sentAccounts, chunks, skippedNoPwd := a.sendOnlineChunks(accounts, gameAddr, chunk, interval, "batch_add")
	sent := len(sentAccounts)
	// 2026-09-23 R3：显式上线 = 用户让它干活 → 解除人工暂停（只解除本次真的下发成功的号）。
	a.resumeAccounts(sentAccounts)
	a.Store.LogEvent(map[string]any{"type": "api", "action": "robots_batch", "sub": "online",
		"zone": zoneArg, "requested": len(accounts), "sent": sent, "chunks": chunks,
		"interval_ms": interval})
	msg := "已分 " + itoa(chunks) + " 批下发上线（" + itoa(sent) + "/" + itoa(len(accounts)) + " 个）"
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
		"skipped_no_password": skippedNoPwd,
		"zone":                zoneArg, "game_addr": gameAddr,
	})
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
