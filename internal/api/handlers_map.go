package api

import (
	"net/http"
)

// handleMapGrid 取某地图的**网格 + 地图名 + 当前在该图的在线机器人**（地图可视化页用）。
//
//	GET /api/map/grid?mapid=11
//
// 网格来自游戏配置目录的阻挡文件（map.csv → map_file/blockfile/<file>）。
func (a *API) handleMapGrid(w http.ResponseWriter, r *http.Request) {
	mapid := toInt(r.URL.Query().Get("mapid"), 0)
	if mapid <= 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少 mapid"})
		return
	}
	name := a.Maps.Name(mapid)
	if name == "" {
		name = a.Grids.MapName(mapid)
	}
	resp := map[string]any{
		"ok": true, "mapid": mapid, "name": name,
		"grid_cell":  a.Cfg.GridCell,
		"available":  a.Grids.Available(),
		"config_dir": a.Grids.ConfigDir(),
	}
	if !a.Grids.Available() {
		resp["ok"] = false
		resp["msg"] = "地图网格数据不可用：请用 --game-config 指定含 map.csv 与 map_file/blockfile 的游戏配置目录"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	grid, err := a.Grids.Grid(mapid)
	if err != nil {
		resp["ok"] = false
		resp["msg"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["grid"] = grid
	writeJSON(w, http.StatusOK, resp)
}
