package api

import (
	"net/http"
	"path/filepath"
	"strings"

	"zyctrlcenter/internal/services/zones"
)

// ---------------------------------------------------------------- 多区配置（服/区，单进程切换式）

// zoneSnapshots 区列表（面板用）：注册表里的区 + 是否当前区 + 该区的目标地址/编码。
func (a *API) zoneSnapshots() []map[string]any {
	serverKey, zoneKey := a.Zones.CurrentKeys()
	out := make([]map[string]any, 0)
	for _, z := range a.Zones.Flat() {
		out = append(out, map[string]any{
			"key": z.Key, "server_key": z.ServerKey, "server_name": z.ServerName,
			"zone_key": z.ZoneKey, "name": z.DisplayName(),
			"addr": z.Addr(), "host": z.Host, "port": z.Port, "coding": z.Coding,
			"note":    z.Note,
			"current": z.ServerKey == serverKey && z.ZoneKey == zoneKey,
		})
	}
	return out
}

// currentZone 当前区（无区时返回空对象）。
func (a *API) currentZone() map[string]any {
	cur, ok := a.Zones.Current()
	if !ok {
		return map[string]any{}
	}
	return map[string]any{
		"key": cur.Key, "server_key": cur.ServerKey, "server_name": cur.ServerName,
		"zone_key": cur.ZoneKey, "name": cur.DisplayName(),
		"addr": cur.Addr(), "host": cur.Host, "port": cur.Port, "coding": cur.Coding,
		"note": cur.Note, "current": true,
	}
}

func (a *API) currentZoneKey() string {
	if cur, ok := a.Zones.Current(); ok {
		return cur.Key
	}
	return ""
}

// syncZoneTag 把「当前区」同步到控制通道的事件标记（切区/改配置后调用）。
// 单进程模式下只有一条通道：标记决定事件/日志里显示哪个区。
func (a *API) syncZoneTag() {
	key := a.currentZoneKey()
	a.Ctrl.SetZone(key)
	a.Log.Printf("[ZONE] 当前区切换为 %s（控制通道事件标记已同步）", key)
}

// robotConfigPath 机器人 config.py 路径（单部署目录）。
func (a *API) robotConfigPath() string {
	return filepath.Join(a.Cfg.DeployDir, "script", "config.py")
}

// handleConfigGet 多区配置总览。
func (a *API) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	serverKey, zoneKey := a.Zones.CurrentKeys()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"servers":      a.Zones.Servers(),
		"zones":        a.zoneSnapshots(),
		"current":      a.currentZone(),
		"current_keys": map[string]any{"server": serverKey, "zone": zoneKey},
		"zones_file":   a.Zones.Path(),
		"updated_at":   a.Zones.UpdatedAt(),
		"defaults": map[string]any{
			"ctrl_port":      a.Cfg.CtrlPort,
			"deploy_dir":     a.Cfg.DeployDir,
			"config_path":    a.robotConfigPath(),
			"codings":        []string{zones.CodingUTF8, zones.CodingGBK}, // 默认值在首位
			"default_coding": zones.DefaultCoding,
		},
		"robot_config_hint": "「应用到机器人」会把该区的 ip/port/PROTOCOL_CODING 写进 " +
			a.robotConfigPath() + "（只改这三个键、写前自动备份），重启机器人后生效",
	})
}

// handleConfigSwitch 切换当前区（低风险：只改"当前选择" + 事件标记，不碰机器人）。
func (a *API) handleConfigSwitch(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	serverKey, zoneKey := toStr(body["server"]), toStr(body["zone"])
	if key := toStr(body["key"]); key != "" { // 也支持直接给全局 key "服/区"
		serverKey, zoneKey = splitZoneKey(key)
	}
	if zoneKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少 zone（或 key）"})
		return
	}
	flat, err := a.Zones.Switch(serverKey, zoneKey)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.syncZoneTag()
	a.Store.LogEvent(map[string]any{"type": "api", "action": "zone_switch",
		"zone": flat.Key, "addr": flat.Addr(), "coding": flat.Coding})
	// 2026-09-22 文案纠正：旧文案让用户点「应用到机器人」，但本部署的 config.py
	// ip/port 是环境变量表达式（ROBOT_ZONE_IP/PORT），写值工具会跳过 → 点了没效果。
	// 真正生效路径 = 切区 + 重启机器人（中控启动机器人时按当前区注入环境变量）。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "zone": flat.Key,
		"current": a.currentZone(),
		"msg": "已切换到区 " + flat.Key + "（" + flat.Addr() + " / " + flat.Coding + "）；" +
			"要让机器人真正登录过去：**重启机器人进程**（中控会按当前区自动注入 " +
			"ROBOT_ZONE_IP/PORT；本部署 config.py 的 ip/port 是环境变量表达式，" +
			"「应用到机器人」不会改写它们）"})
}

// handleConfigServerUpsert 新增/更新服（含其下区列表全量覆盖）。
func (a *API) handleConfigServerUpsert(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	raw, ok := body["server"].(map[string]any)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少 server 对象"})
		return
	}
	srv := zones.Server{
		Key:    toStr(raw["key"]),
		Name:   toStr(raw["name"]),
		Host:   toStr(raw["host"]),
		Coding: toStr(raw["coding"]),
		Note:   toStr(raw["note"]),
	}
	if arr, ok := raw["zones"].([]any); ok {
		for _, item := range arr {
			zm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			srv.Zones = append(srv.Zones, zones.Zone{
				Key:    toStr(zm["key"]),
				Name:   toStr(zm["name"]),
				Port:   toInt(zm["port"], 0),
				Coding: toStr(zm["coding"]),
				Note:   toStr(zm["note"]),
			})
		}
	}
	saved, added, err := a.Zones.UpsertServer(srv)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.syncZoneTag()
	a.Store.LogEvent(map[string]any{"type": "api", "action": "zone_server_save",
		"server": saved.Key, "host": saved.Host, "added": added, "zones": len(saved.Zones)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "server": saved, "added": added,
		"msg": "已保存服 " + saved.Key + "（" + saved.Host + "，含 " + itoa(len(saved.Zones)) + " 个区）"})
}

// handleConfigZoneUpsert 新增/更新区。
func (a *API) handleConfigZoneUpsert(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	serverKey := toStr(body["server"])
	raw, ok := body["zone"].(map[string]any)
	if !ok || serverKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "需要 server 与 zone 对象"})
		return
	}
	saved, added, err := a.Zones.UpsertZone(serverKey, zones.Zone{
		Key:    toStr(raw["key"]),
		Name:   toStr(raw["name"]),
		Port:   toInt(raw["port"], 0),
		Coding: toStr(raw["coding"]),
		Note:   toStr(raw["note"]),
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.syncZoneTag()
	a.Store.LogEvent(map[string]any{"type": "api", "action": "zone_save",
		"zone": zones.MakeKey(serverKey, saved.Key), "port": saved.Port, "added": added})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "zone": saved, "added": added,
		"msg": "已保存区 " + zones.MakeKey(serverKey, saved.Key)})
}

// handleConfigServerDelete 删除服（连同其区）。
func (a *API) handleConfigServerDelete(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	serverKey := toStr(body["server"])
	if serverKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少 server"})
		return
	}
	removed, err := a.Zones.RemoveServer(serverKey)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.syncZoneTag()
	a.Store.LogEvent(map[string]any{"type": "api", "action": "zone_server_delete",
		"server": serverKey, "removed": removed})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed,
		"msg": map[bool]string{true: "已删除服 " + serverKey, false: "服不存在: " + serverKey}[removed]})
}

// handleConfigZoneDelete 删除区。
func (a *API) handleConfigZoneDelete(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	serverKey := toStr(body["server"])
	zoneKey := toStr(body["zone"])
	if serverKey == "" || zoneKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "需要 server 与 zone"})
		return
	}
	removed, err := a.Zones.RemoveZone(serverKey, zoneKey)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.syncZoneTag()
	a.Store.LogEvent(map[string]any{"type": "api", "action": "zone_delete",
		"zone": zones.MakeKey(serverKey, zoneKey), "removed": removed})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed,
		"msg": map[bool]string{true: "已删除区 " + serverKey + "/" + zoneKey,
			false: "区不存在: " + serverKey + "/" + zoneKey}[removed]})
}

// handleConfigApply 把某区（默认当前区）写进机器人 config.py；可选重启机器人进程。
//
// 只改 ip / port / PROTOCOL_CODING 三个键，写前自动备份（见 zones.ApplyRobotConfig）。
func (a *API) handleConfigApply(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	zoneKey := zoneKeyOf(r, body)
	if zoneKey == "" {
		zoneKey = a.currentZoneKey()
	} else if zoneKey != a.currentZoneKey() {
		// 允许直接对指定区应用：顺便把它设为当前区（保持"当前区 = 机器人登录的区"一致）
		if _, err := a.Zones.Switch(splitZoneKey(zoneKey)); err == nil {
			a.syncZoneTag()
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
	}
	if zoneKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "没有可用区（先配置服/区）"})
		return
	}
	flat, ok := a.Zones.FlatOf(splitZoneKey(zoneKey))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "区不存在: " + zoneKey})
		return
	}

	confPath := a.robotConfigPath()
	res, err := zones.ApplyRobotConfig(confPath, flat.Host, flat.Port, flat.Coding)
	if err != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "zone_apply",
			"zone": flat.Key, "applied": false, "err": err.Error()})
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "zone": flat.Key,
			"config_path": confPath, "msg": err.Error()})
		return
	}

	restarted := false
	restartErr := ""
	if toBool(body["restart_robot"], false) {
		if err := a.Proc.Restart(a.Cfg.KillRobots); err != nil {
			restartErr = err.Error()
		} else {
			restarted = true
		}
	}

	msg := "已应用到机器人 config.py"
	if !res.Applied {
		msg = "内容已一致，无需改写"
	}
	if len(res.Skipped) > 0 {
		// 值是表达式（如环境变量）：跳过不改，提示人工处理，避免改坏生产配置。
		// 2026-09-22 补充：本部署（single_robot_zy）的 ip/port 就是
		// `_os.environ.get("ROBOT_ZONE_IP") or ...` 形式 —— 正确做法是"切区 + 重启机器人"
		// （中控启动时按当前区注入 ROBOT_ZONE_IP/PORT），不需要改 config.py。
		msg += "；已跳过（值为表达式，未改动）: " + strings.Join(res.Skipped, ", ") +
			"。本部署走环境变量注入：**切区后重启机器人进程**即可生效，无需改 config.py"
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "zone_apply", "zone": flat.Key,
		"addr": flat.Addr(), "coding": flat.Coding, "applied": res.Applied,
		"changed": res.Changed, "backup": res.Backup, "restarted": restarted, "restart_err": restartErr})
	resp := map[string]any{
		"ok": true, "zone": flat.Key, "addr": flat.Addr(), "coding": flat.Coding,
		"config_path": confPath, "result": res, "restarted": restarted,
	}
	switch {
	case restarted:
		resp["msg"] = msg + "（" + flat.Addr() + "）；已重启机器人进程"
	case restartErr != "":
		resp["msg"] = msg + "（" + flat.Addr() + "）；重启机器人失败: " + restartErr
	default:
		resp["msg"] = msg + "（" + flat.Addr() + "）；如需生效请重启机器人进程"
	}
	writeJSON(w, http.StatusOK, resp)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
