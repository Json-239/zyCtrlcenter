// 游荡（random_walk）下发：**通用入口**（孵化去半月岛、以后其它巡游图都用它）。
//
// 机器人端契约（`deploy/single_robot_zy/script/random_walk.py:dispatch_cmd`，2026-09-22 核实）：
//
//	{"cmd":"random_walk","mapid":6,"chain":<导航链>,"range":[[x1,y1],[x2,y2]],
//	 "min_bot_dist":200,"minutes":30,"accounts":["a@x.com"]}
//	{"cmd":"random_walk_stop","accounts":["a@x.com"]}
//
//   - `chain` **必须带**：机器人端只认命令里的链数据（npcs/map_grids/dijkstra），
//     组装的活由 `Payloads.Walk` 干（目标图网格 + 跨图路由缺一就报错，不发空命令）；
//   - `accounts` **必须带**：机器人端不带 accounts = 对**所有号**生效（现场 100+ 号会一起游荡），
//     所以这里直接拒绝空账号；
//   - `range` / `min_bot_dist` 不给就用机器人端默认（`[[200,200],[3300,3000]]` / 200）；
//   - `minutes` 可选（0/缺省=不限），目前机器人端**不解析**（仅透传，留作限时游荡）；
//     孵化链路的限时由 `hatch_start.max_minutes` 负责（中控侧到期下发 hatch_stop）。
//
// 下发给谁：只发给**在线**的号；不在线/无状态记录的号逐个给"为什么没下发"（不静默）。
package api

import (
	"fmt"
	"net/http"
	"strings"

	"zyctrlcenter/internal/state"
)

// handleRandomWalk 下发游荡（写接口）。
//
//	POST /api/random_walk
//	{"accounts":["a@x.com"],"mapid":6,"minutes":30,"range":[[200,200],[3300,3000]]}
func (a *API) handleRandomWalk(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	mapid := toInt(body["mapid"], 0)
	if mapid <= 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false,
			"msg": "mapid 必填（目标图；例：6=半月岛、10=大唐东野林）"})
		return
	}
	accounts := normAccounts(bodyAccounts(body))
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false,
			"msg": "accounts 为空：游荡必须指定账号（机器人端不带账号=对所有号生效，这里拦掉）"})
		return
	}
	if a.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "事件通道不可用（命令下发不了）"})
		return
	}
	// 载荷载荷先取齐：链数据缺网格/缺路由就是硬失败，一条命令都不发
	chain, err := a.chainPayloads().Walk(mapid)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false,
			"msg": "游荡导航数据不可用，未下发任何命令：" + err.Error()})
		return
	}

	minutes := toInt(body["minutes"], 0)
	if minutes < 0 {
		minutes = 0
	}
	online, failed := a.onlineAccounts(accounts)
	resp := map[string]any{
		"ok": false, "mapid": mapid, "map_name": a.mapName(mapid), "minutes": minutes,
		"sent": 0, "failed": len(failed), "failed_accounts": failed,
	}
	if len(online) == 0 {
		resp["msg"] = "没有可下发的号：" + failedText(failed)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	cmd := map[string]any{"cmd": "random_walk", "mapid": mapid, "chain": chain, "accounts": online}
	if minutes > 0 {
		cmd["minutes"] = minutes
	}
	if rg, ok := body["range"]; ok && rg != nil { // 可选：透传给机器人（不给用它的默认范围）
		cmd["range"] = rg
	}
	if d := toInt(body["min_bot_dist"], 0); d > 0 {
		cmd["min_bot_dist"] = d
	}
	// 可选：游荡档位（机器人端 ROAM_PROFILES 注册表，缺省 default）。
	//   default = 既有"账号级随机"（真人挂机感）；dense = 密集游荡（几乎不停，孵化踩暗雷/野外打怪）；
	//   后续要加"野外挂机/挖宝/挖矿/打猎"等用途时，机器人端注册新档位即可，这里透传字符串。
	//   未知档位由机器人端回退 default 并记日志（不会把号卡死）。
	if md, _ := body["mode"].(string); strings.TrimSpace(md) != "" {
		cmd["mode"] = strings.TrimSpace(md)
	}
	ok := a.Events.SendCmd(cmd, "random_walk")
	sent := 0
	if ok {
		sent = len(online)
	}
	resp["ok"], resp["sent"] = ok, sent
	msg := fmt.Sprintf("已下发游荡：%d 个号 → 图 %d%s", sent, mapid, a.mapNameSuffix(mapid))
	if minutes > 0 {
		msg += fmt.Sprintf("（限时 %d 分钟）", minutes)
	}
	if len(failed) > 0 {
		msg += "；未下发 " + fmt.Sprintf("%d 个：%s", len(failed), failedText(failed))
	}
	resp["msg"] = okMsg(ok, msg)
	a.Store.LogEvent(map[string]any{"type": "api", "action": "random_walk", "zone": a.currentZoneKey(),
		"mapid": mapid, "minutes": minutes, "accounts": online, "failed": len(failed), "sent": ok})
	writeJSON(w, http.StatusOK, resp)
}

// handleRandomWalkStop 停止游荡（写接口）。
//
//	POST /api/random_walk/stop
//	{"accounts":["a@x.com"]}   // 省略 accounts = 停全部（与 /api/stop 同口径）
func (a *API) handleRandomWalkStop(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	accounts := normAccounts(bodyAccounts(body))
	if a.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "事件通道不可用（命令下发不了）"})
		return
	}
	if len(accounts) == 0 { // 不带账号：机器人端 random_walk_stop 不带 accounts = 停所有号的游荡
		ok := a.Events.SendCmd(map[string]any{"cmd": "random_walk_stop"}, "random_walk_stop")
		a.Store.LogEvent(map[string]any{"type": "api", "action": "random_walk_stop",
			"zone": a.currentZoneKey(), "scope": "all", "sent": ok})
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "sent": 0, "failed": 0,
			"msg": okMsg(ok, "已下发停止游荡（全部账号）")})
		return
	}
	online, failed := a.onlineAccounts(accounts)
	resp := map[string]any{"ok": false, "sent": 0, "failed": len(failed), "failed_accounts": failed}
	if len(online) == 0 {
		resp["msg"] = "没有可下发的号：" + failedText(failed)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ok := a.Events.SendCmd(map[string]any{"cmd": "random_walk_stop", "accounts": online}, "random_walk_stop")
	sent := 0
	if ok {
		sent = len(online)
	}
	resp["ok"], resp["sent"] = ok, sent
	msg := fmt.Sprintf("已下发停止游荡：%d 个号", sent)
	if len(failed) > 0 {
		msg += "；未下发 " + fmt.Sprintf("%d 个：%s", len(failed), failedText(failed))
	}
	resp["msg"] = okMsg(ok, msg)
	a.Store.LogEvent(map[string]any{"type": "api", "action": "random_walk_stop", "zone": a.currentZoneKey(),
		"accounts": online, "failed": len(failed), "sent": ok})
	writeJSON(w, http.StatusOK, resp)
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
