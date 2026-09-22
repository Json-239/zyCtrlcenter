// 任务号 → 任务名（只读游戏配置；给面板把编号翻成人话）。
package api

import (
	"net/http"
	"strconv"
	"strings"
)

// handleTaskNames 任务号 → 任务名（只读；名字来自游戏配置 <GameConfigDir>/task/*.xml）。
//
//	GET /api/tasknames            → {ok, count, dir, available, names:{"2019508":"捉鬼",…}}
//	GET /api/tasknames?id=2019508 → 额外回带 name（单个查询）
//
// 目录不可用（没配游戏配置）时返回空表：前端回退显示编号，不报错（与 /api/maps 同款口径）。
func (a *API) handleTaskNames(w http.ResponseWriter, r *http.Request) {
	if a.TaskNames == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": 0, "available": false,
			"names": map[string]string{}, "msg": "任务名表未启用（未配置游戏配置目录）"})
		return
	}
	names := map[string]string{}
	for idx, name := range a.TaskNames.All() {
		names[strconv.Itoa(idx)] = name
	}
	resp := map[string]any{
		"ok": true, "count": len(names), "available": a.TaskNames.Available(),
		"dir": a.TaskNames.Dir(), "names": names,
	}
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		if n, err := strconv.Atoi(id); err == nil {
			resp["id"] = n
			resp["name"] = a.TaskNames.Lookup(n)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
