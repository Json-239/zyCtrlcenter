// 摆摊配置（离线摆摊·单次时长）：GET 默认值 / POST 校验 1..480 / 落盘 / 计划文件同步（只改一个字段）。
//
// 用户需求（2026-09-28）：面板可配"摆摊挂多久"；生效范围为**下一次**挂摊
// （机器人端开摊时才读计划文件，已挂的摊要收摊重挂）。
package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// boothPlanJSON 一份摆摊计划文件（形状与生产 booth_selftest.json 一致；其它字段用于验证同步时原样保留）。
const boothPlanJSON = `{
 "enabled": true,
 "account": "robot0001032@xy3.com",
 "mapid": 11,
 "offline_minutes": 480,
 "_note": "离线摆摊·8小时"
}
`

// isolateBoothPaths 把候选计划文件路径全部隔离到临时目录（避免测试碰到真实仓库/参考项目副本），
// 并在 DeployDir 与仓库副本位各放一份计划文件；返回两份路径。
func isolateBoothPaths(t *testing.T, env *testEnv) []string {
	t.Helper()
	env.cfg.BaseDir = t.TempDir() // 候选 2/3 都从这里推导 → 与真实仓库/参考项目隔离
	paths := []string{
		filepath.Join(env.cfg.DeployDir, "script", "booth_selftest.json"),
		filepath.Join(env.cfg.BaseDir, "deploy", "zones", "prod-240-2300", "script", "booth_selftest.json"),
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(boothPlanJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

// 默认值 + 校验：GET 默认 480；>480 / <1 / 缺字段都明确拒绝；>480 的文案说明 8h 上限。
func TestBoothConfigDefaultsAndValidation(t *testing.T) {
	env := newTestEnv(t, "")
	isolateBoothPaths(t, env)

	body := getJSON(t, env.srv.URL+"/api/booth/config")
	if body["ok"] != true || body["offline_minutes"] != float64(480) {
		t.Fatalf("默认配置应为 480 分钟: %v", body)
	}
	if body["min"] != float64(1) || body["max"] != float64(480) {
		t.Fatalf("应回带允许范围 1..480: %v", body)
	}

	_, over := postJSON(t, env.srv.URL+"/api/booth/config", map[string]any{"offline_minutes": 500}, nil)
	if over["ok"] != false || !strings.Contains(toStrAny(over["msg"]), "480") {
		t.Fatalf(">480 必须拒绝并说明上限: %v", over)
	}
	_, zero := postJSON(t, env.srv.URL+"/api/booth/config", map[string]any{"offline_minutes": 0}, nil)
	if zero["ok"] != false {
		t.Fatalf("<1 必须拒绝: %v", zero)
	}
	_, missing := postJSON(t, env.srv.URL+"/api/booth/config", map[string]any{}, nil)
	if missing["ok"] != false {
		t.Fatalf("缺字段必须拒绝: %v", missing)
	}
}

// 保存 + 同步：POST 60 → 落盘 data/booth_config.json；两份计划文件的 offline_minutes=60，
// 其它字段（enabled/account/mapid/_note）原样保留；再 GET 读回 60。
func TestBoothConfigSaveAndPlanSync(t *testing.T) {
	env := newTestEnv(t, "")
	plans := isolateBoothPaths(t, env)

	_, body := postJSON(t, env.srv.URL+"/api/booth/config", map[string]any{"offline_minutes": 60}, nil)
	if body["ok"] != true || body["saved"] != float64(60) || body["plan_synced"] != true {
		t.Fatalf("保存 60 应成功且同步: %v", body)
	}
	if got := asSlice(body["plan_files"]); len(got) != 2 {
		t.Fatalf("应同步两份计划文件（DeployDir + 仓库副本）: %v", body["plan_files"])
	}
	// 落盘文件
	raw, err := os.ReadFile(filepath.Join(env.cfg.DataDir, "booth_config.json"))
	if err != nil {
		t.Fatalf("配置应落盘: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg["offline_minutes"] != float64(60) {
		t.Fatalf("落盘内容应含 offline_minutes=60: %s", raw)
	}
	// 计划文件：只改一个字段，其它原样
	for _, p := range plans {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var plan map[string]any
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatalf("%s 同步后应为合法 JSON: %v", p, err)
		}
		if plan["offline_minutes"] != float64(60) {
			t.Fatalf("%s 的 offline_minutes 应为 60: %s", p, raw)
		}
		if plan["enabled"] != true || plan["account"] != "robot0001032@xy3.com" ||
			plan["mapid"] != float64(11) || plan["_note"] != "离线摆摊·8小时" {
			t.Fatalf("%s 其它字段应原样保留: %s", p, raw)
		}
	}
	// 再 GET：读回落盘值
	again := getJSON(t, env.srv.URL+"/api/booth/config")
	if again["offline_minutes"] != float64(60) {
		t.Fatalf("GET 应读回落盘的 60: %v", again)
	}
}

// 计划文件缺失：配置照常保存、plan_synced=false 不算失败；损坏的配置读取时回默认 480。
func TestBoothConfigPlanMissingAndCorruptConfig(t *testing.T) {
	env := newTestEnv(t, "")
	env.cfg.BaseDir = t.TempDir() // 无计划文件的孤立目录（DeployDir 也是空 temp）

	_, body := postJSON(t, env.srv.URL+"/api/booth/config", map[string]any{"offline_minutes": 30}, nil)
	if body["ok"] != true || body["plan_synced"] != false {
		t.Fatalf("计划文件缺失时配置仍应保存、plan_synced=false: %v", body)
	}

	// 写坏配置 → GET 回默认 480
	cfgPath := filepath.Join(env.cfg.DataDir, "booth_config.json")
	if err := os.WriteFile(cfgPath, []byte("{ 坏 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := getJSON(t, env.srv.URL+"/api/booth/config")
	if got["offline_minutes"] != float64(480) {
		t.Fatalf("损坏配置应回默认 480: %v", got)
	}
}
