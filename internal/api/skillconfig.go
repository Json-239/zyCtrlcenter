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
// POST 的 strategy 是"字段级覆盖"：键 "种族:性别"（如 immortal:male）→ "种族" → "default"；
// 每个键的值两种形态都收 —— 字符串（模式）或对象 {"mode": "...", "magics": [...]}
// （magics 只对 magic_random 有意义；见 strategyValue）。
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
			if sv, ok := strategyValue(v); ok {
				cfg.Strategy[k] = sv
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

// strategyValue 归一化面板提交的一个策略值（key 是 "种族:性别" / "种族" / "default"）：
//   - 字符串（旧格式=模式）→ 原样收；空串跳过（沿用"空值不改"的语义）
//   - 对象 {"mode": "...", "magics": [...]} → mode 必须是非空字符串；magics 收敛成 []string
//     （非字符串项丢掉；空了就不写该字段 —— 机器人端无 magics 的 magic_random 回落 random）
//   - 其它形态（数字/数组/null…）→ 不识别，跳过该键（保持原值）
//
// 返回的 any 直接存进 SkillConfig.Strategy（map[string]any），JSON 序列化后下发机器人。
func strategyValue(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil, false
		}
		return t, true
	case map[string]any:
		mode, _ := t["mode"].(string)
		if mode == "" {
			return nil, false
		}
		out := map[string]any{"mode": mode}
		if arr, ok := t["magics"].([]any); ok {
			magics := make([]string, 0, len(arr))
			for _, x := range arr {
				if s, ok := x.(string); ok && s != "" {
					magics = append(magics, s)
				}
			}
			if len(magics) > 0 {
				out["magics"] = magics
			}
		}
		return out, true
	}
	return nil, false
}
