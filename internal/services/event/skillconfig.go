package event

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// 2026-09-23 技能策略配置（前端「模块地图 · 战斗」卡可配）。
//
// 存储：<DataDir>/skill_config.json（保存即下发；机器人连上(hello)时自动补发一份）。
// 机器人端：client.py 的 skill_config 命令 → robot.m_skill_cfg →
// skill_attack.pick_skill 按策略选技能（没收到配置 = 用内置默认，行为与下面的默认值一致）。
type SkillConfig struct {
	// Enabled 技能攻击总开关（false = 全部普攻）。
	Enabled bool `json:"enabled"`
	// Scenes 分场景开关：ghost(抓鬼)/wild(野外)/story(剧情) —— false 的场景用普攻。
	Scenes map[string]bool `json:"scenes"`
	// Guardian 守护(召唤兽)策略：attack=默认普通攻击（当前唯一实现）。
	Guardian string `json:"guardian"`
	// Strategy 种族[×性别] → 选技能模式：best_damage(最优伤害) / magic_random(指定系别随机) /
	// special_random(特殊技能随机) / random(可用随机)。
	//   · key 依次尝试 "种族:性别"（如 immortal:male）→ "种族"（不分性别的通用档）→ "default" 兜底；
	//   · 值两种形态：字符串（模式）或对象 {"mode": "...", "magics": ["雷系","风系"]}
	//     —— magics 只对 magic_random 有意义（机器人端无匹配系别时回落 random）；
	//   · 未识别的模式按 random 处理。
	Strategy map[string]any `json:"strategy"`
	// Special 特殊技能池（默认 901 初露锋芒 / 902 一石二鸟 —— 每号都有的新手伤害技）。
	Special []int `json:"special"`
}

// DefaultSkillConfig 与机器人端内置默认一致（改了这里要同步 skill_attack 的兜底）。
func DefaultSkillConfig() SkillConfig {
	return SkillConfig{
		Enabled:  true,
		Scenes:   map[string]bool{"ghost": true, "wild": true, "story": false},
		Guardian: "attack",
		Strategy: map[string]any{
			"immortal": "best_damage",    // 仙族：门派纯伤害技最优
			"human":    "special_random", // 人族：特殊技能随机
			"demon":    "special_random", // 魔族：特殊技能随机
			"default":  "random",         // 兜底
		},
		Special: []int{901, 902},
	}
}

var skillCfgMu sync.Mutex

// skillConfigPath <DataDir>/skill_config.json。
func (h *Handler) skillConfigPath() string {
	dir := "data"
	if h.Cfg != nil && h.Cfg.DataDir != "" {
		dir = h.Cfg.DataDir
	}
	return filepath.Join(dir, "skill_config.json")
}

// LoadSkillConfig 读配置；缺失/损坏 → 默认值（并补齐 nil 字段）。
func (h *Handler) LoadSkillConfig() SkillConfig {
	cfg := DefaultSkillConfig()
	skillCfgMu.Lock()
	b, err := os.ReadFile(h.skillConfigPath())
	skillCfgMu.Unlock()
	if err == nil && len(b) > 0 {
		var got SkillConfig
		if json.Unmarshal(b, &got) == nil {
			// map/slice 是整体覆盖：未给的键要补默认，避免 nil
			if got.Scenes == nil {
				got.Scenes = cfg.Scenes
			}
			if got.Strategy == nil {
				got.Strategy = cfg.Strategy
			} else {
				for k, v := range cfg.Strategy {
					if _, ok := got.Strategy[k]; !ok {
						got.Strategy[k] = v
					}
				}
			}
			if len(got.Special) == 0 {
				got.Special = cfg.Special
			}
			if got.Guardian == "" {
				got.Guardian = cfg.Guardian
			}
			cfg = got
		}
	}
	return cfg
}

// SaveSkillConfig 落盘（保存即生效于下次下发）。
func (h *Handler) SaveSkillConfig(cfg SkillConfig) error {
	skillCfgMu.Lock()
	defer skillCfgMu.Unlock()
	p := h.skillConfigPath()
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// PushSkillConfig 把当前配置下发给机器人（在线号下一场战斗即生效）。
func (h *Handler) PushSkillConfig() bool {
	cfg := h.LoadSkillConfig()
	return h.SendCmd(map[string]any{"cmd": "skill_config", "config": cfg}, "skill_config")
}
