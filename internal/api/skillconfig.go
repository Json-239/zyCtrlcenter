package api

import (
	"net/http"
)

// 2026-09-23 技能策略配置接口（前端「模块地图 · 战斗」卡）：
//
//	GET  /api/skill_config   读当前配置（默认值 + 已保存覆盖）
//	POST /api/skill_config   保存并下发（在线机器人下一场战斗生效；
//	                         未连接时机器人连上(hello)会自动补发）
//
// 定义与持久化在 internal/services/event/skillconfig.go。
func (a *API) handleSkillConfigGet(w http.ResponseWriter, r *http.Request) {
	if a.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "事件处理器未装配"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": a.Events.LoadSkillConfig()})
}

func (a *API) handleSkillConfigPost(w http.ResponseWriter, r *http.Request) {
	if a.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "事件处理器未装配"})
		return
	}
	body := readBody(r)
	cfg := a.Events.LoadSkillConfig() // 字段级覆盖：未给的保持
	if v, ok := body["enabled"].(bool); ok {
		cfg.Enabled = v
	}
	if v := toStr(body["guardian"]); v != "" {
		cfg.Guardian = v
	}
	if m, ok := body["scenes"].(map[string]any); ok {
		for k, v := range m {
			if b, ok := v.(bool); ok {
				cfg.Scenes[k] = b
			}
		}
	}
	if m, ok := body["strategy"].(map[string]any); ok {
		for k, v := range m {
			if s := toStr(v); s != "" {
				cfg.Strategy[k] = s
			}
		}
	}
	if arr, ok := body["special"].([]any); ok {
		spec := make([]int, 0, len(arr))
		for _, v := range arr {
			if n := toInt(v, 0); n > 0 {
				spec = append(spec, n)
			}
		}
		if len(spec) > 0 {
			cfg.Special = spec
		}
	}
	if err := a.Events.SaveSkillConfig(cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "保存失败: " + err.Error()})
		return
	}
	sent := a.Events.PushSkillConfig()
	msg := "技能策略已保存"
	if sent {
		msg += "并已下发机器人（下一场战斗生效）"
	} else {
		msg += "（机器人通道未连接，连上后自动补发）"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": cfg, "msg": msg})
}
