// 游荡（random_walk）下发：**通用入口**（孵化去半月岛、以后其它巡游图都用它）。
//
// 机器人端契约（`deploy/single_robot_zy/script/random_walk.py:dispatch_cmd`，2026-09-22 核实）：
//
//		{"cmd":"random_walk","mapid":6,"chain":<导航链>,"range":[[x1,y1],[x2,y2]],
//		 "min_bot_dist":200,"mode":"dense","minutes":30,"accounts":["a@x.com"]}
//		{"cmd":"random_walk","mapid":"random","maps":[6,17,34,40],...}   // 随机图（机器人端每号抽一张）
//		{"cmd":"random_walk_stop","accounts":["a@x.com"]}
//
//	  - `mapid` **必填**：数字图号（具体图）或字符串 `"random"`（随机图 —— 机器人端从链数据
//	    有网格的图里随机挑一张，**每号不同**，避免全号扎堆同一张图）；
//	  - `maps` **可选白名单**（图号数组/逗号串）：随机图时只从白名单里挑（如孵化图 6/17/34/40）；
//	    指定图时必须在内（不在就明确拒绝，不静默换图）。白名单里有图没有寻路网格 → 拒绝；
//	  - `mode` **档位**（机器人端 ROAM_PROFILES 注册表）：default / dense / gather(占位)…；
//	    另外接受 `"mode":"random"` 作为随机图的等价别名（此时档位可用 `profile` 字段指定）；
//	  - `minutes` **限时**（分钟；0=不限）：机器人端到点自动 random_walk_stop（2026-09-22 落地）；
//	  - `chain` **必须带**：机器人端只认命令里的链数据（npcs/map_grids/dijkstra），
//	    组装的活由 `Payloads.Walk`/`WalkRandom` 干（缺网格/缺路由就报错，不发空命令）；
//	  - `accounts` **必须带**：机器人端不带 accounts = 对**所有号**生效（现场 100+ 号会一起游荡），
//	    所以这里直接拒绝空账号；
//	  - `range` / `min_bot_dist` 不给就用机器人端默认（无 range 时机器人端按目标图网格自动算）。
//
// 下发给谁：只发给**在线**的号；不在线/无状态记录的号逐个给"为什么没下发"（不静默）。
package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/state"
)

// handleRandomWalk 下发游荡（写接口）。
//
//	POST /api/random_walk
//	{"accounts":["a@x.com"],"mapid":6,"minutes":30,"range":[[200,200],[3300,3000]]}
//	{"accounts":["a@x.com"],"mapid":"random","maps":[6,17,34,40],"mode":"dense","minutes":60}
//	{"accounts":["a@x.com"],"mode":"random","profile":"dense"}   // mode=random 等价别名
//
// 真正的下发在 DispatchRoamEx —— **与游荡池 keeper（internal/services/roampool）共用同一实现**，
// 避免"面板下发"与"池子自动派发"两套逻辑漂移。
func (a *API) handleRandomWalk(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	mapidRaw := body["mapid"]
	modeStr := strings.TrimSpace(toStr(body["mode"]))
	mapsList, mapsErr := parseMapList(body["maps"])
	if mapsErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "maps 白名单非法：" + mapsErr.Error()})
		return
	}
	// 随机图：mapid="random"（推荐）或 mode="random"（等价别名，档位改用 profile 字段）。
	isRandom := isRandomMapVal(mapidRaw)

	// 2026-09-22 世界图过滤(与机器人端 config.robot_roam_world_maps 同口径):
	//   随机图游荡默认只从"世界地图可获取的图"里抽(排掉房间/店铺/洞穴这类小图, 如回春药铺 616)。
	//   调用方显式给了 maps 白名单 → 以调用方为准(不覆盖)。
	if mapsErr == nil && isRandom && len(mapsList) == 0 && len(a.Cfg.RoamWorldMaps) > 0 {
		mapsList = append([]int(nil), a.Cfg.RoamWorldMaps...)
	}
	// 2026-09-22 游荡排除图（用户口径）：幽冥界 24 是抓鬼专属 —— 无论随机白名单还是
	// 显式 maps，都从这里剔除（与机器人端 config.robot_roam_exclude_maps 同口径，双保险）。
	if len(a.Cfg.RoamExcludeMaps) > 0 && len(mapsList) > 0 {
		kept := make([]int, 0, len(mapsList))
		for _, m := range mapsList {
			if !containsInt(a.Cfg.RoamExcludeMaps, m) {
				kept = append(kept, m)
			}
		}
		mapsList = kept
	}
	profile := modeStr
	if isRandomWord(modeStr) {
		if mapidRaw != nil && toInt(mapidRaw, 0) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false,
				"msg": "mapid 与 mode=random 冲突：随机图不要再指定 mapid（档位用 mode 正常传，如 \"dense\"）"})
			return
		}
		isRandom = true
		profile = strings.TrimSpace(toStr(body["profile"])) // 别名写法下档位走 profile（可选）
	}
	mapid := 0
	if !isRandom {
		mapid = toInt(mapidRaw, 0)
		if mapid <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false,
				"msg": "mapid 必填（数字图号；或 \"random\"=随机图。例：6=半月岛、10=大唐东野林）"})
			return
		}
		// 2026-09-22 游荡排除图（用户口径）：显式选到幽冥界(24) 直接拒绝（不动号上任务）
		if containsInt(a.Cfg.RoamExcludeMaps, mapid) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false,
				"msg": fmt.Sprintf("目标图 %d 不在游荡范围（幽冥界为抓鬼专属）", mapid)})
			return
		}
		if len(mapsList) > 0 && !containsInt(mapsList, mapid) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false,
				"msg": fmt.Sprintf("目标图 %d 不在 maps 白名单 %v 内（要么去掉 maps，要么把该图加进白名单）",
					mapid, mapsList)})
			return
		}
	}
	accounts := normAccounts(bodyAccounts(body))
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false,
			"msg": "accounts 为空：游荡必须指定账号（机器人端不带账号=对所有号生效，这里拦掉）"})
		return
	}
	// 下发（与游荡池 keeper 共用；HTTP 响应与此前完全一致）
	req := RoamReq{
		Accounts: accounts, Random: isRandom, MapID: mapid, Maps: mapsList,
		Mode: profile, Minutes: toInt(body["minutes"], 0), MinBotDist: toInt(body["min_bot_dist"], 0),
	}
	if rg, ok := body["range"]; ok && rg != nil {
		req.Range = rg // 可选：透传给机器人（不给就按目标图网格自动算）
	}
	res := a.DispatchRoamEx(req)
	if res.Err != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": res.Err})
		return
	}
	resp := map[string]any{
		"ok": res.OK, "mapid": res.MapID, "random": res.Random, "minutes": res.Minutes,
		"sent": res.Sent, "failed": len(res.Failed), "failed_accounts": res.Failed,
	}
	if !res.Random {
		resp["map_name"] = a.mapName(res.MapID)
	}
	if len(mapsList) > 0 {
		resp["maps"] = mapsList
	}
	resp["msg"] = res.Msg
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- 下发/停止（HTTP 接口 与 游荡池 keeper 共用）

// RoamReq 一次"下发游荡"的请求（HTTP 解析结果 / 游荡池 keeper 直接构造）。
type RoamReq struct {
	Accounts []string
	// MapID >0 = 指定图（机器人端跨图走到该图再随机走）；Random=true 时忽略。
	MapID int
	// Random = mapid "random"：机器人端**每号自抽**一张（避免全号扎堆同一张图）。
	Random bool
	// Maps 白名单（随机图限定抽签范围；指定图时必须包含该图 —— 调用方先校验）。
	Maps []int
	// Mode 档位（机器人端 ROAM_PROFILES：default/dense/gather(占位)…；空/random = 机器人端默认档）。
	Mode string
	// Minutes 限时（分钟；0=不限；负数按 0）。
	Minutes int
	// Range / MinBotDist 可选透传（不给就由机器人端/目标图网格自行计算）。
	Range      any
	MinBotDist int
}

// RoamResult 下发结果（HTTP 响应与 keeper 共用）。
type RoamResult struct {
	OK      bool // 命令是否下发成功（通道可用）
	Random  bool
	MapID   int
	Minutes int
	Sent    int
	Online  []string
	Failed  []map[string]any
	// Err 参数/载荷错误：**一条命令都没发**（HTTP 侧直接回 {ok:false,msg}）。
	Err string
	// Msg 人话结果（"已下发游荡：…" / "没有可下发的号：…"）。
	Msg string
}

// DispatchRoam 下发游荡（**供 HTTP 接口与游荡池 keeper 共用**的简化入口）。
//
//	mapid 是 int（指定图）或 "random"/"rand"/"any"（随机图，每号自抽）。
//	keeper 用它派发"池子名额"，面板接口用下面的 DispatchRoamEx（带白名单/档位/限时/range）。
func (a *API) DispatchRoam(accounts []string, mapid any, mode string, minutes int) RoamResult {
	req := RoamReq{Accounts: accounts, Mode: mode, Minutes: minutes}
	if isRandomMapVal(mapid) {
		req.Random = true
	} else {
		req.MapID = toInt(mapid, 0)
	}
	return a.DispatchRoamEx(req)
}

// DispatchRoamEx 下发游荡（完整形态）：链载荷先取齐（硬失败 = 一条命令都不发）→ 只发在线号 → SendCmd。
func (a *API) DispatchRoamEx(req RoamReq) RoamResult {
	res := RoamResult{Random: req.Random, MapID: req.MapID, Minutes: req.Minutes, Failed: []map[string]any{}}
	if req.Minutes < 0 {
		res.Minutes = 0
	}
	if !req.Random {
		if res.MapID <= 0 {
			res.Err = "mapid 必填（数字图号；或 \"random\"=随机图。例：6=半月岛、10=大唐东野林）"
			return res
		}
		if len(req.Maps) > 0 && !containsInt(req.Maps, res.MapID) {
			res.Err = fmt.Sprintf("目标图 %d 不在 maps 白名单 %v 内（要么去掉 maps，要么把该图加进白名单）",
				res.MapID, req.Maps)
			return res
		}
	}
	accounts := normAccounts(req.Accounts)
	if len(accounts) == 0 {
		res.Err = "accounts 为空：游荡必须指定账号（机器人端不带账号=对所有号生效，这里拦掉）"
		return res
	}
	if a.Events == nil {
		res.Err = "事件通道不可用（命令下发不了）"
		return res
	}
	// 链载荷先取齐：链数据缺网格/缺路由就是硬失败，一条命令都不发
	var chain *chainlib.Chain
	var err error
	if req.Random {
		chain, err = a.chainPayloads().WalkRandom(req.Maps)
	} else {
		chain, err = a.chainPayloads().Walk(res.MapID)
	}
	if err != nil {
		res.Err = "游荡导航数据不可用，未下发任何命令：" + err.Error()
		return res
	}

	online, failed := a.onlineAccounts(accounts)
	res.Failed = failed
	if len(online) == 0 {
		res.Msg = "没有可下发的号：" + failedText(failed)
		return res
	}

	cmd := map[string]any{"cmd": "random_walk", "chain": chain, "accounts": online}
	if req.Random {
		cmd["mapid"] = "random" // 机器人端 resolve_roam_target 解析（每号随机抽签）
	} else {
		cmd["mapid"] = res.MapID
	}
	if len(req.Maps) > 0 {
		cmd["maps"] = req.Maps // 白名单：随机图时限定抽签范围；指定图时是"校验已过"的约束
	}
	// 限时总是带（0=不限）：机器人端据此刷新/清除倒计时（缺省不带 = 保持已有倒计时）
	cmd["minutes"] = res.Minutes
	if req.Range != nil { // 可选：透传给机器人（不给就按目标图网格自动算）
		cmd["range"] = req.Range
	}
	if req.MinBotDist > 0 {
		cmd["min_bot_dist"] = req.MinBotDist
	}
	// 可选：游荡档位（机器人端 ROAM_PROFILES 注册表，缺省 default）。
	//   default = 既有"账号级随机"（真人挂机感）；dense = 密集游荡（几乎不停，孵化踩暗雷/野外打怪）；
	//   gather  = 采集占位档（未实现：机器人端明确回退 default 并记日志，不静默当 default 跑）；
	//   后续要加"野外挂机/挖宝/打猎"等用途时，机器人端注册新档位即可，这里透传字符串。
	//   未知档位由机器人端回退 default 并记日志（不会把号卡死）。
	if m := strings.TrimSpace(req.Mode); m != "" && !isRandomWord(m) {
		cmd["mode"] = m
	}
	ok := a.Events.SendCmd(cmd, "random_walk")
	res.OK = ok
	if ok {
		res.Sent = len(online)
	}
	res.Online = online
	var where string
	if req.Random {
		where = "随机图"
		if len(req.Maps) > 0 {
			where += fmt.Sprintf("（白名单 %v）", req.Maps)
		}
	} else {
		where = fmt.Sprintf("图 %d%s", res.MapID, a.mapNameSuffix(res.MapID))
	}
	msg := fmt.Sprintf("已下发游荡：%d 个号 → %s", res.Sent, where)
	if res.Minutes > 0 {
		msg += fmt.Sprintf("（限时 %d 分钟）", res.Minutes)
	}
	if len(failed) > 0 {
		msg += "；未下发 " + fmt.Sprintf("%d 个：%s", len(failed), failedText(failed))
	}
	res.Msg = okMsg(ok, msg)
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "random_walk", "zone": a.currentZoneKey(),
			"mapid": res.MapID, "random": req.Random, "maps": req.Maps, "minutes": res.Minutes,
			"accounts": online, "failed": len(failed), "sent": ok})
	}
	return res
}

// parseMapList 解析 maps 白名单：接受 [6,17,34,40] / ["6","17"] / "6,17,34,40" / 6。
//
// 任一项非法（非数字 / <=0）都**明确报错**（不静默丢项：白名单少一张图会变成隐性偏差）；
// 重复项去重；没给（nil）返回 nil,nil。
func parseMapList(raw any) ([]int, error) {
	if raw == nil {
		return nil, nil
	}
	var items []any
	switch t := raw.(type) {
	case []any:
		items = t
	case string:
		for _, p := range splitList(t) {
			items = append(items, p)
		}
	case float64, int:
		items = []any{t}
	default:
		return nil, fmt.Errorf("不支持的类型 %T（应为图号数组，如 [6,17,34,40]）", raw)
	}
	out := make([]int, 0, len(items))
	for _, it := range items {
		s := strings.TrimSpace(toStr(it))
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("图号 %q 非法（应为正整数）", toStr(it))
		}
		if !containsInt(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("maps 为空（要么去掉 maps，要么给图号列表，如 [6,17,34,40]）")
	}
	return out, nil
}

// isRandomWord 是不是"随机图"请求词（用于 mode 字段的等价别名）。大小写不敏感。
func isRandomWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "random", "rand", "any":
		return true
	}
	return false
}

// isRandomMapVal mapid 字段是不是"随机图"请求（字符串 "random"/"rand"/"any"）。
func isRandomMapVal(v any) bool {
	return isRandomWord(toStr(v))
}

// containsInt 整数切片是否含 v（白名单校验用）。
func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// handleRandomWalkStop 停止游荡（写接口）。
//
//	POST /api/random_walk/stop
//	{"accounts":["a@x.com"]}   // 省略 accounts = 停全部（与 /api/stop 同口径）
//
// 真正的下发在 StopRoam（**与游荡池 keeper 共用**）。
func (a *API) handleRandomWalkStop(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	res := a.StopRoam(bodyAccounts(body))
	if res.Err != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": res.Err})
		return
	}
	resp := map[string]any{"ok": res.OK, "sent": res.Sent, "failed": len(res.Failed)}
	if !res.All {
		resp["failed_accounts"] = res.Failed
	}
	resp["msg"] = res.Msg
	writeJSON(w, http.StatusOK, resp)
}

// StopResult 停止游荡的结果（HTTP 响应与 keeper 共用）。
type StopResult struct {
	All    bool // 没带账号 = 停全部（机器人端 random_walk_stop 不带 accounts）
	OK     bool // 命令是否下发成功（通道可用）
	Sent   int
	Online []string
	Failed []map[string]any
	// Err 事件通道不可用（命令下发不了）
	Err string
	Msg string
}

// StopRoam 停止游荡（**供 HTTP 接口与游荡池 keeper 共用**）。
//
// accounts 为空 = 停**所有号**的游荡（与 POST /api/random_walk/stop 同口径）；
// 只给在线号下发，没下发的逐个带原因（不静默）。
func (a *API) StopRoam(accounts []string) StopResult {
	res := StopResult{Failed: []map[string]any{}}
	if a.Events == nil {
		res.Err = "事件通道不可用（命令下发不了）"
		return res
	}
	accs := normAccounts(accounts)
	if len(accs) == 0 { // 不带账号：机器人端 random_walk_stop 不带 accounts = 停所有号的游荡
		res.All = true
		res.OK = a.Events.SendCmd(map[string]any{"cmd": "random_walk_stop"}, "random_walk_stop")
		res.Msg = okMsg(res.OK, "已下发停止游荡（全部账号）")
		if a.Store != nil {
			a.Store.LogEvent(map[string]any{"type": "api", "action": "random_walk_stop",
				"zone": a.currentZoneKey(), "scope": "all", "sent": res.OK})
		}
		return res
	}
	online, failed := a.onlineAccounts(accs)
	res.Failed = failed
	if len(online) == 0 {
		res.Msg = "没有可下发的号：" + failedText(failed)
		return res
	}
	res.OK = a.Events.SendCmd(map[string]any{"cmd": "random_walk_stop", "accounts": online}, "random_walk_stop")
	if res.OK {
		res.Sent = len(online)
	}
	res.Online = online
	msg := fmt.Sprintf("已下发停止游荡：%d 个号", res.Sent)
	if len(failed) > 0 {
		msg += "；未下发 " + fmt.Sprintf("%d 个：%s", len(failed), failedText(failed))
	}
	res.Msg = okMsg(res.OK, msg)
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "random_walk_stop", "zone": a.currentZoneKey(),
			"accounts": online, "failed": len(failed), "sent": res.OK})
	}
	return res
}

// onlineAccounts 把账号分成"可下发（在线）"与"没下发（附人话原因）"。
func (a *API) onlineAccounts(accs []string) (online []string, failed []map[string]any) {
	failed = []map[string]any{}
	for _, acc := range accs {
		var r state.Robot
		ok := false
		if a.St != nil {
			r, ok = a.St.Get(acc)
		}
		switch {
		case !ok:
			failed = append(failed, map[string]any{"account": acc,
				"msg": "没有状态记录（没上线过，或中控刚重启还没收到心跳）"})
		case !r.Online:
			failed = append(failed, map[string]any{"account": acc, "msg": "不在线（先在面板上线，或等自动重登）"})
		default:
			online = append(online, acc)
		}
	}
	return online, failed
}

// failedText 把"未下发原因"拼成一行（msg/toast 用）。
func failedText(failed []map[string]any) string {
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		parts = append(parts, toStr(f["account"])+"："+toStr(f["msg"]))
	}
	return strings.Join(parts, "；")
}

// mapName 地图中文名（表里没有返回空串）。
func (a *API) mapName(mapid int) string {
	if a.Maps == nil {
		return ""
	}
	return a.Maps.Name(mapid)
}

// mapNameSuffix 地图名的括号形式（没有名字就是空串）。
func (a *API) mapNameSuffix(mapid int) string {
	if n := a.mapName(mapid); n != "" {
		return "（" + n + "）"
	}
	return ""
}
