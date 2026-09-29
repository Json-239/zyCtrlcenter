// 摆摊配置（离线摆摊·单次时长）：GET 默认值 / POST 校验 1..480 / 落盘 / 计划文件同步（只改一个字段）
// + 一键下发/停止（2026-09-29：POST /api/booth/start|stop）。
//
// 用户需求（2026-09-28）：面板可配"摆摊挂多久"；生效范围为**下一次**挂摊
// （机器人端开摊时才读计划文件，已挂的摊要收摊重挂）。
// 用户需求（2026-09-29）：面板要一个"一键下发摆摊指令"的入口（启动/停止）。
package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
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

// ---------------------------------------------------------------- 一键下发 / 停止（2026-09-29）

// installBoothWalkChain 装"摆摊送图"导航夹具：图 6（半月岛，当作号出发图）+ 图 11（摆摊目标图）。
func installBoothWalkChain(t *testing.T, env *testEnv) {
	t.Helper()
	testsupport.InstallChainFixture(t, env.cfg.ChainDir, "newbie_full", "chains/booth_walk.mini.json")
}

// readBoothPlan 读一份计划文件为 map（断言用）。
func readBoothPlan(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s 应是合法 JSON: %v", path, err)
	}
	return m
}

// assertBoothPlanStarted 断言"启动"写入的受控字段（enabled=true + 固定口径）。
func assertBoothPlanStarted(t *testing.T, plan map[string]any, acc string, cellX, cellY, offMin float64) {
	t.Helper()
	if plan["enabled"] != true {
		t.Fatalf("enabled 应为 true: %v", plan)
	}
	if plan["account"] != acc || plan["mapid"] != float64(11) {
		t.Fatalf("account/mapid 不符: %v", plan)
	}
	cell := asSlice(plan["cell"])
	if len(cell) != 2 || cell[0] != cellX || cell[1] != cellY {
		t.Fatalf("cell 应为 [%v,%v]: %v", cellX, cellY, plan["cell"])
	}
	if plan["booth_name"] != "杂货小摊" {
		t.Fatalf("摊名不符: %v", plan["booth_name"])
	}
	if items := asSlice(plan["up_items"]); len(items) != 3 {
		t.Fatalf("默认应上架 3 件货: %v", plan["up_items"])
	}
	if plan["hold_sec"] != float64(120) || plan["max_open_retry"] != float64(3) || plan["map_wait_sec"] != float64(900) {
		t.Fatalf("hold_sec/max_open_retry/map_wait_sec 不符: %v", plan)
	}
	if plan["offline_minutes"] != offMin {
		t.Fatalf("offline_minutes 应为 %v（随配置）: %v", offMin, plan["offline_minutes"])
	}
}

// 号已在图 11：只写计划（两份副本、全量受控字段）、不发任何命令。
func TestBoothStartAlreadyInMapWritesPlanOnly(t *testing.T) {
	env := newTestEnv(t, "")
	plans := isolateBoothPaths(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "robot0001032@xy3.com"
	feedRobot(t, env, acc, nil)
	env.st.Update(acc, func(r *state.Robot) { r.MapID = 11 })

	_, body := postJSON(t, env.srv.URL+"/api/booth/start", map[string]any{"account": acc}, nil)
	if body["ok"] != true || body["action"] != "already_in_map" {
		t.Fatalf("号已在图 11 应 ok + already_in_map: %v", body)
	}
	if got := asSlice(body["plan_written"]); len(got) != 2 {
		t.Fatalf("应写两份计划副本: %v", body["plan_written"])
	}
	for _, p := range plans {
		assertBoothPlanStarted(t, readBoothPlan(t, p), acc, 1385, 1545, 480)
	}
	// 号已在图 → 不该发命令
	if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
		t.Fatalf("号已在图 11 不该发任何命令: %v", cmd)
	}

	// GET 摘要：当前计划已启用、带 cell
	sum, _ := getJSON(t, env.srv.URL+"/api/booth/config")["plan"].(map[string]any)
	if sum["enabled"] != true || sum["account"] != acc || sum["mapid"] != float64(11) {
		t.Fatalf("GET plan 摘要不符: %v", sum)
	}
	if cell := asSlice(sum["cell"]); len(cell) != 2 || cell[0] != float64(1385) {
		t.Fatalf("GET plan 摘要应带 cell: %v", sum)
	}
}

// 号不在图 11（在线）：先送图（游荡通道，命令带图 11 网格 + maps=[11]）→ 写计划。
func TestBoothStartSendsMapWhenElsewhere(t *testing.T) {
	env := newTestEnv(t, "")
	plans := isolateBoothPaths(t, env)
	installBoothWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "robot0001032@xy3.com"
	feedRobot(t, env, acc, nil)
	env.st.Update(acc, func(r *state.Robot) { r.MapID = 10 })

	_, body := postJSON(t, env.srv.URL+"/api/booth/start",
		map[string]any{"account": acc, "cell": []any{1400, 1500}}, nil)
	if body["ok"] != true || body["action"] != "sent_map" {
		t.Fatalf("号在图 10 应 ok + sent_map: %v", body)
	}
	// 送到图 11 的 random_walk 命令（复用游荡通道）
	cmd := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd["cmd"]) != "random_walk" || cmd["mapid"] != float64(11) {
		t.Fatalf("应下发 random_walk → 图 11: %v", cmd)
	}
	if got := asSlice(cmd["maps"]); len(got) != 1 || got[0] != float64(11) {
		t.Fatalf("定向派发应显式带 maps=[11]: %v", cmd["maps"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	grids, _ := chain["map_grids"].(map[string]any)
	if _, ok := grids["11"]; !ok {
		t.Fatalf("链载荷必须有图 11 寻路网格: %v", chain["map_grids"])
	}
	// 计划已写（cell 用入参覆盖）
	for _, p := range plans {
		assertBoothPlanStarted(t, readBoothPlan(t, p), acc, 1400, 1500, 480)
	}
}

// 号离线：只写计划（不送图）、action=offline、hint 提示先拉起；不发任何命令。
func TestBoothStartOfflineWritesPlanAndPrompts(t *testing.T) {
	env := newTestEnv(t, "")
	plans := isolateBoothPaths(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "robot0001032@xy3.com"
	feedRobot(t, env, acc, map[string]any{"online": false})

	_, body := postJSON(t, env.srv.URL+"/api/booth/start", map[string]any{"account": acc}, nil)
	if body["ok"] != true || body["action"] != "offline" {
		t.Fatalf("号离线应 ok + offline（计划照写）: %v", body)
	}
	if !strings.Contains(toStrAny(body["hint"]), "拉起") {
		t.Fatalf("hint 应提示先拉起号: %v", body["hint"])
	}
	for _, p := range plans {
		if readBoothPlan(t, p)["enabled"] != true {
			t.Fatalf("离线也应写 enabled=true（拉起后可自动开摊）: %s", p)
		}
	}
	if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
		t.Fatalf("离线号不该收到任何命令: %v", cmd)
	}
}

// 送图失败（链数据没装 → Walk(11) 硬失败）：放弃写计划（不留 enabled=true 的挂空计划）。
func TestBoothStartSendMapFailureKeepsPlanUntouched(t *testing.T) {
	env := newTestEnv(t, "")
	plans := isolateBoothPaths(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	before := make([][]byte, len(plans))
	for i, p := range plans {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[i] = raw
	}

	acc := "robot0001032@xy3.com"
	feedRobot(t, env, acc, nil)
	env.st.Update(acc, func(r *state.Robot) { r.MapID = 10 })

	_, body := postJSON(t, env.srv.URL+"/api/booth/start", map[string]any{"account": acc}, nil)
	if body["ok"] != false || body["action"] != "send_map_failed" {
		t.Fatalf("送图硬失败应 ok=false + send_map_failed: %v", body)
	}
	for i, p := range plans {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(before[i]) {
			t.Fatalf("送图失败时计划文件不应被改动: %s", p)
		}
	}
}

// offline_minutes 随配置：先 POST 240 → 启动写出的计划 offline_minutes=240。
func TestBoothStartUsesConfiguredOfflineMinutes(t *testing.T) {
	env := newTestEnv(t, "")
	plans := isolateBoothPaths(t, env)
	acc := "robot0001032@xy3.com"
	feedRobot(t, env, acc, nil)
	env.st.Update(acc, func(r *state.Robot) { r.MapID = 11 })

	if _, cfg := postJSON(t, env.srv.URL+"/api/booth/config", map[string]any{"offline_minutes": 240}, nil); cfg["ok"] != true {
		t.Fatalf("配置保存失败: %v", cfg)
	}
	if _, body := postJSON(t, env.srv.URL+"/api/booth/start", map[string]any{"account": acc}, nil); body["ok"] != true {
		t.Fatalf("启动失败: %v", body)
	}
	for _, p := range plans {
		if got := readBoothPlan(t, p)["offline_minutes"]; got != float64(240) {
			t.Fatalf("计划应随配置写 240: %v", got)
		}
	}
}

// 入参校验：非法 cell 明确拒绝（不写计划）；_note 被更新（最近一次操作可读）。
func TestBoothStartValidatesCell(t *testing.T) {
	env := newTestEnv(t, "")
	isolateBoothPaths(t, env)

	_, bad := postJSON(t, env.srv.URL+"/api/booth/start",
		map[string]any{"account": "robot0001032@xy3.com", "cell": []any{0, 0}}, nil)
	if bad["ok"] != false || bad["action"] != "bad_request" {
		t.Fatalf("非法 cell 应明确拒绝: %v", bad)
	}
}

// 未知字段保留：旧计划里手工加的字段（如 open_wait_ms）在启动重写后仍在（只覆盖受控字段）。
func TestBoothStartPreservesUnknownPlanFields(t *testing.T) {
	env := newTestEnv(t, "")
	env.cfg.BaseDir = t.TempDir()
	paths := []string{
		filepath.Join(env.cfg.DeployDir, "script", "booth_selftest.json"),
		filepath.Join(env.cfg.BaseDir, "deploy", "zones", "prod-240-2300", "script", "booth_selftest.json"),
	}
	custom := `{"enabled": false, "account": "old@x.com", "mapid": 10, "open_wait_ms": 3000, "_note": "手工调优"}`
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(custom), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	acc := "robot0001032@xy3.com"
	feedRobot(t, env, acc, nil)
	env.st.Update(acc, func(r *state.Robot) { r.MapID = 11 })

	_, body := postJSON(t, env.srv.URL+"/api/booth/start", map[string]any{"account": acc}, nil)
	if body["ok"] != true {
		t.Fatalf("启动失败: %v", body)
	}
	plan := readBoothPlan(t, paths[0])
	if plan["open_wait_ms"] != float64(3000) {
		t.Fatalf("未识别字段应原样保留: %v", plan)
	}
	if plan["enabled"] != true || plan["account"] != acc || plan["mapid"] != float64(11) {
		t.Fatalf("受控字段应被覆盖: %v", plan)
	}
}

// 停止：写 enabled=false（双份）；在线 → action=stopped；离线 → action=offline + 提示拉起；
// 其它字段（account/mapid/货单）原样保留。
func TestBoothStopDisablesPlan(t *testing.T) {
	env := newTestEnv(t, "")
	plans := isolateBoothPaths(t, env)
	// 用一份带全量字段的旧计划覆盖夹具：验证停止只翻 enabled、不动其它字段
	full := `{"enabled": true, "account": "robot0001032@xy3.com", "mapid": 11, "cell": [1385,1545],
 "booth_name": "杂货小摊", "up_items": [{"item_index":170001,"name":"云母粉","price":1000}], "_note": "旧计划"}`
	for _, p := range plans {
		if err := os.WriteFile(p, []byte(full), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	acc := "robot0001032@xy3.com"
	feedRobot(t, env, acc, nil)
	env.st.Update(acc, func(r *state.Robot) { r.MapID = 11 })

	_, body := postJSON(t, env.srv.URL+"/api/booth/stop", map[string]any{"account": acc}, nil)
	if body["ok"] != true || body["action"] != "stopped" {
		t.Fatalf("在线停止应 ok + stopped: %v", body)
	}
	for _, p := range plans {
		plan := readBoothPlan(t, p)
		if plan["enabled"] != false {
			t.Fatalf("停止后 enabled 应为 false: %v", plan)
		}
		if plan["account"] != acc || plan["mapid"] != float64(11) {
			t.Fatalf("停止不该动其它字段: %v", plan)
		}
		if items := asSlice(plan["up_items"]); len(items) != 1 {
			t.Fatalf("停止不该动货单: %v", plan["up_items"])
		}
	}

	// 离线停止：提示需拉起号收摊（不自动拉起）
	feedRobot(t, env, acc, map[string]any{"online": false})
	_, body2 := postJSON(t, env.srv.URL+"/api/booth/stop", map[string]any{"account": acc}, nil)
	if body2["ok"] != true || body2["action"] != "offline" {
		t.Fatalf("离线停止应 ok + offline: %v", body2)
	}
	if !strings.Contains(toStrAny(body2["hint"]), "拉起") {
		t.Fatalf("hint 应提示需拉起号收摊: %v", body2["hint"])
	}
	for _, p := range plans {
		if readBoothPlan(t, p)["enabled"] != false {
			t.Fatalf("离线也应写 enabled=false: %s", p)
		}
	}
}
