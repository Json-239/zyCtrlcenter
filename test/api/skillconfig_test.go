// 技能策略配置（GET/POST /api/skill_config）：strategy 的两种值形态 + 缺省合并。
//
// 2026-09-23 契约（前端「模块地图 · 战斗」卡 + 机器人端 skill_attack.pick_skill）：
//
//	· key 依次退化 "种族:性别" → "种族" → "default"；
//	· 值可以是字符串（模式）或对象 {"mode": "...", "magics": [...]}；
//	· magics 只对 magic_random 有意义；面板提交对象值时必须能原样回读
//	  （前端 saveSkill() 拿回读结果做"是否被丢弃"的校验）。
package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 面板提交对象值 → 原样存/回读（含 magics），并落盘（重启后仍带 magics）。
func TestSkillConfigStrategyObjectRoundTrip(t *testing.T) {
	env := newTestEnv(t, "")
	url := env.srv.URL + "/api/skill_config"

	// 面板实际提交的形态：magic_random 带 magics；其它模式是字符串
	_, body := postJSON(t, url, map[string]any{
		"strategy": map[string]any{
			"immortal:male":   map[string]any{"mode": "magic_random", "magics": []any{"雷系", "风系"}},
			"immortal:female": map[string]any{"mode": "magic_random", "magics": []any{"水系", "火系"}},
			"human:female":    "best_damage",
			"default":         "random",
		},
	}, nil)
	if body["ok"] != true {
		t.Fatalf("保存应成功: %v", body)
	}

	// POST 回读：对象值原样带 magics（前端的"是否被丢弃"校验就靠它）
	got, _ := body["config"].(map[string]any)["strategy"].(map[string]any)
	m, _ := got["immortal:male"].(map[string]any)
	if m["mode"] != "magic_random" {
		t.Fatalf("immortal:male 应是对象且 mode=magic_random: %v", got["immortal:male"])
	}
	magics, _ := m["magics"].([]any)
	if len(magics) != 2 || magics[0] != "雷系" || magics[1] != "风系" {
		t.Fatalf("magics 应原样回带: %v", m["magics"])
	}
	if got["human:female"] != "best_damage" {
		t.Fatalf("字符串形态应原样保留: %v", got["human:female"])
	}

	// GET 回读 + 落盘：重启（新建环境复用同目录）后对象值还在
	get := getJSON(t, url)
	gstrat, _ := get["config"].(map[string]any)["strategy"].(map[string]any)
	gm, _ := gstrat["immortal:female"].(map[string]any)
	if gm["mode"] != "magic_random" {
		t.Fatalf("GET 应回带对象值: %v", gstrat["immortal:female"])
	}

	raw, err := os.ReadFile(filepath.Join(env.cfg.DataDir, "skill_config.json"))
	if err != nil {
		t.Fatalf("配置文件应落盘: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("落盘文件应是合法 JSON: %v", err)
	}
	ssaved, _ := saved["strategy"].(map[string]any)
	if _, ok := ssaved["immortal:male"].(map[string]any); !ok {
		t.Fatalf("落盘文件里对象值应保持对象形态: %v", ssaved["immortal:male"])
	}
}

// 旧格式（值全是字符串）能读能存，不被新代码破坏。
func TestSkillConfigLegacyStringStrategy(t *testing.T) {
	env := newTestEnv(t, "")
	legacy := `{"enabled":true,"scenes":{"ghost":true,"wild":true,"story":false},` +
		`"guardian":"attack","strategy":{"immortal":"best_damage","human":"special_random",` +
		`"demon":"special_random","default":"random"},"special":[901,902]}`
	if err := os.WriteFile(filepath.Join(env.cfg.DataDir, "skill_config.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	url := env.srv.URL + "/api/skill_config"
	st, _ := getJSON(t, url)["config"].(map[string]any)["strategy"].(map[string]any)
	if st["immortal"] != "best_damage" {
		t.Fatalf("旧格式字符串值应原样读出: %v", st)
	}

	// 旧格式再存一次（只改一个键）→ 别的键保持
	_, body := postJSON(t, url, map[string]any{
		"strategy": map[string]any{"human": "best_damage"},
	}, nil)
	if body["ok"] != true {
		t.Fatalf("旧格式保存应成功: %v", body)
	}
	got, _ := body["config"].(map[string]any)["strategy"].(map[string]any)
	if got["human"] != "best_damage" || got["immortal"] != "best_damage" || got["default"] != "random" {
		t.Fatalf("字段级覆盖：只改给的键，其它保持: %v", got)
	}
}

// 未识别形态跳过该键（不覆盖原值）；magics 里的非字符串项被丢掉；空 magics 不写该字段。
func TestSkillConfigStrategyBadShapesSkipped(t *testing.T) {
	env := newTestEnv(t, "")
	url := env.srv.URL + "/api/skill_config"

	postJSON(t, url, map[string]any{
		"strategy": map[string]any{"immortal": "magic_random"},
	}, nil) // 先立一个可被覆盖的原值

	_, body := postJSON(t, url, map[string]any{
		"strategy": map[string]any{
			"immortal":      123,                                                                        // 数字 → 跳过（保住原值）
			"human":         nil,                                                                        // null → 跳过
			"demon":         []any{"random"},                                                            // 数组 → 跳过
			"default":       map[string]any{"magics": []any{"雷系"}},                                      // 对象缺 mode → 跳过
			"immortal:male": map[string]any{"mode": "magic_random", "magics": []any{"雷系", 7, "", "风系"}}, // 非法项丢掉
			"human:male":    map[string]any{"mode": "magic_random"},                                     // 无 magics → 不写该字段
		},
	}, nil)
	if body["ok"] != true {
		t.Fatalf("坏形态不该整体失败: %v", body)
	}
	got, _ := body["config"].(map[string]any)["strategy"].(map[string]any)

	if got["immortal"] != "magic_random" {
		t.Fatalf("数字形态应跳过、保住原值: %v", got["immortal"])
	}
	if got["human"] != "special_random" {
		t.Fatalf("null 形态应跳过 → 默认值应还在: %v", got["human"])
	}
	if got["demon"] != "special_random" {
		t.Fatalf("数组形态应跳过: %v", got["demon"])
	}
	if got["default"] != "random" {
		t.Fatalf("缺 mode 的对象应跳过: %v", got["default"])
	}
	im, _ := got["immortal:male"].(map[string]any)
	magics, _ := im["magics"].([]any)
	if len(magics) != 2 || magics[0] != "雷系" || magics[1] != "风系" {
		t.Fatalf("magics 应只留非空字符串: %v", im["magics"])
	}
	hm, _ := got["human:male"].(map[string]any)
	if hm["mode"] != "magic_random" {
		t.Fatalf("无 magics 的对象仍要收下 mode: %v", got["human:male"])
	}
	if _, ok := hm["magics"]; ok {
		t.Fatalf("空 magics 不应写该字段: %v", hm)
	}
}

// 缺键补齐：只存了部分键时，默认键（含"种族:性别"这类新键之外的老键）仍在。
func TestSkillConfigStrategyDefaultsFilled(t *testing.T) {
	env := newTestEnv(t, "")
	if err := os.WriteFile(filepath.Join(env.cfg.DataDir, "skill_config.json"), []byte(`{"strategy":{"immortal:male":"random"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := getJSON(t, env.srv.URL+"/api/skill_config")["config"].(map[string]any)["strategy"].(map[string]any)
	for _, k := range []string{"immortal:male", "immortal", "human", "demon", "default"} {
		if st[k] == nil {
			t.Fatalf("缺的键应补上默认（机器人端要能退化到）：%v", st)
		}
	}
	if st["immortal:male"] != "random" || st["immortal"] != "best_damage" {
		t.Fatalf("已存的保持、缺的补默认: %v", st)
	}
}
