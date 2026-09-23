// 分享日常（大唐神捕，shenbu）的中控侧契约（2026-09-23，方案 §7 / 清单 G3-G5）：
//
//   - 命令：share_daily_start{accounts, share_key, chain_id, chain, daily_limit, done?}
//     —— 载荷 = 基座 newbie_full（坐标/网格/路由）+ shenbu_nav 声明（task_order 全 4 个任务号）；
//   - 载荷缺失 → **硬失败**（一条命令都不发）；
//   - 停策略 → 给在跑的号下发 share_daily_stop 收工（与 hatch_stop 同款）；
//   - 补发（/api/intents/restore）同口径：带 share_key/daily_limit/done，多号合并成一条；
//   - GET /api/daily/overview 空值安全（没有心跳 = 空数组，不瞎编）；池视图按 kind 自动获得。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/test/testsupport"
)

// 下发 share_daily_start：命令形状 + 载荷完整性（task_order 全 4 个任务号，坐标/网格/路由从基座复用）。
func TestLaunchShareDailyCarriesPayload(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)

	ok, msg := env.api.LaunchTask(autotask.KindShenbu, []string{"sd1@xy3.com"})
	if !ok {
		t.Fatalf("下发大唐神捕应成功: %s", msg)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" {
		t.Fatalf("应收 share_daily_start: %v", cmd)
	}
	if cmd["share_key"] != "share_daily_大唐神捕" {
		t.Fatalf("应带玩法键 share_key: %v", cmd["share_key"])
	}
	if cmd["chain_id"] != "shenbu_nav" {
		t.Fatalf("应带链声明文件名（无 chain_id 时按文件名补）: %v", cmd["chain_id"])
	}
	if cmd["daily_limit"] != float64(10) {
		t.Fatalf("应带 daily_limit=10（大唐神捕服务端口径）: %v", cmd["daily_limit"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	if len(chain) == 0 {
		t.Fatalf("必须带 chain 载荷（不带机器人拿不到 task_order 与导航）: %v", cmd)
	}
	// R1 防护：task_order 必须列全 4 个分支任务号（少列会被机器人当「链外任务」静默忽略）
	order := asSlice(chain["task_order"])
	want := map[float64]bool{2028301: false, 2028302: false, 2028311: false, 2028399: false}
	for _, it := range order {
		o, _ := it.(map[string]any)
		idx, _ := o["task_index"].(float64)
		if _, known := want[idx]; !known {
			t.Fatalf("task_order 出现未知任务号 %v（应只有 2028301/2028302/2028311/2028399）: %v", idx, o)
		}
		want[idx] = true
	}
	for idx, seen := range want {
		if !seen {
			t.Fatalf("task_order 少列任务号 %v（少列会让后续环节被静默忽略）: %v", idx, order)
		}
	}
	// 坐标/网格/路由从基座 newbie_full 复用（专属文件只放声明）
	npcs, _ := chain["npcs"].(map[string]any)
	if len(npcs) == 0 {
		t.Fatalf("chain.npcs 不能为空（基座链提供）: %v", chain)
	}
	grids, _ := chain["map_grids"].(map[string]any)
	if len(grids) == 0 {
		t.Fatalf("chain.map_grids 不能为空（基座链提供）: %v", chain)
	}
	if dj, _ := chain["dijkstra"].(map[string]any); len(dj) == 0 {
		t.Fatalf("chain.dijkstra 不能为空（基座链提供）: %v", chain)
	}
	if extra := rb.TryReadCmd(200 * time.Millisecond); extra != nil {
		t.Fatalf("只该收到一条 share_daily_start: %v", extra)
	}
}

// 链数据缺失 → 硬失败：ok=false + 明确原因，且**一条命令都不发**。
func TestLaunchShareDailyMissingPayloadFailsLoudly(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	// 刻意不装链数据（基座/声明都不存在）

	ok, msg := env.api.LaunchTask(autotask.KindShenbu, []string{"sd2@xy3.com"})
	if ok {
		t.Fatalf("载荷缺失应硬失败: %s", msg)
	}
	if !strings.Contains(msg, "分享日常链数据不可用") {
		t.Fatalf("报错要说清原因（面板直接显示）: %s", msg)
	}
	if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
		t.Fatalf("硬失败不该发任何命令: %v", cmd)
	}
}

// 停策略 → 给在跑的号下发 share_daily_stop 收工（否则号会一直跑到日限）。
func TestAutoTaskStopSendsShareDailyStop(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)

	if ok, msg := env.api.LaunchTask(autotask.KindShenbu, []string{"sd3@xy3.com"}); !ok {
		t.Fatalf("下发大唐神捕应成功: %s", msg)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["cmd"] != "share_daily_start" {
		t.Fatalf("先应收到 share_daily_start: %v", cmd)
	}
	_, res := postJSON(t, env.srv.URL+"/api/autotask/stop", map[string]any{"kind": "shenbu"}, nil)
	if res["ok"] != true {
		t.Fatalf("停止 shenbu 策略应成功: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_stop" {
		t.Fatalf("停策略应给在跑的号下发 share_daily_stop: %v", cmd)
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != "sd3@xy3.com" {
		t.Fatalf("收工命令应带在跑的账号: %v", cmd["accounts"])
	}
}

// 补发（按意图）与「启动」同口径：带 share_key/daily_limit/done，多号合并成一条。
func TestIntentsRestoreShareDailyMergesAndCarriesKey(t *testing.T) {
	t.Setenv("CTRL_SHARE_DAILY", "1") // 判据开关（默认关）：打开后 45 级 + 未满 → 判 shenbu
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	// 池启用（restorer 补发前有池状态闸：池没启用不自动补发）
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 300, BatchMin: 1, BatchMax: 1, TargetOnline: 10,
	}); err != nil {
		t.Fatalf("启动神捕池失败: %v", err)
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "sd4@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0,
				"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 3, "limit": 10, "state": "RUNNING"}},
			map[string]any{"account": "sd5@xy3.com", "level": 46, "online": true, "state": "IDLE", "task_index": 0,
				"daily": []any{map[string]any{"share_key": "share_daily_大唐神捕", "done": 0, "limit": 10, "state": "IDLE"}}},
		},
		"_zone": testsupportZone()})
	if it, ok := env.ev.Intents.Get("sd4@xy3.com"); !ok || it.Kind != intent.KindShenbu {
		t.Fatalf("45 级 + 神捕未满应登记 shenbu 意图: %+v", it)
	}
	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res["ok"] != true || res["sent"] != float64(2) || res["commands"] != float64(1) {
		t.Fatalf("两个神捕号应合并成 1 条命令（sent=2 账号 / commands=1 命令）: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_大唐神捕" {
		t.Fatalf("补发必须是带 share_key 的 share_daily_start: %v", cmd)
	}
	if cmd["chain_id"] != "shenbu_nav" || cmd["daily_limit"] != float64(10) {
		t.Fatalf("补发同口径：带 chain_id 与 daily_limit: %v", cmd)
	}
	chain, _ := cmd["chain"].(map[string]any)
	if len(asSlice(chain["task_order"])) != 4 {
		t.Fatalf("补发也要带完整 task_order（4 个任务号）: %v", chain["task_order"])
	}
	done, _ := cmd["done"].(map[string]any)
	if done["sd4@xy3.com"] != float64(3) {
		t.Fatalf("补发应带各号已做次数（重新下发不丢进度）: %v", done)
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 2 {
		t.Fatalf("一条命令应覆盖两个账号: %v", cmd["accounts"])
	}
	if extra := rb.TryReadCmd(200 * time.Millisecond); extra != nil {
		t.Fatalf("合并后机器人只该收到一条命令: %v", extra)
	}
}

// GET /api/daily/overview：没有心跳 = 空数组 + 玩法键（空值安全，不瞎编）。
func TestDailyOverviewEmptySafe(t *testing.T) {
	env := newTestEnv(t, "")
	res := getJSON(t, env.srv.URL+"/api/daily/overview")
	if res["ok"] != true {
		t.Fatalf("接口应 ok: %v", res)
	}
	if rows := asSlice(res["rows"]); len(rows) != 0 {
		t.Fatalf("没有心跳时应给空数组: %v", res["rows"])
	}
	keys := asSlice(res["share_keys"])
	if len(keys) != 1 || keys[0] != "share_daily_大唐神捕" {
		t.Fatalf("应带玩法键（前端表头用）: %v", res["share_keys"])
	}
}

// GET /api/daily/overview：带心跳 daily 块的号 → queue/current/order 结构齐全（order 先空）。
func TestDailyOverviewShowsQueueAndCurrent(t *testing.T) {
	t.Setenv("CTRL_SHARE_DAILY", "1")
	env := newTestEnv(t, "")
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": "sd6@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 4, "limit": 10, "state": "RUNNING"}}},
		"_zone": testsupportZone()})

	res := getJSON(t, env.srv.URL+"/api/daily/overview")
	rows := asSlice(res["rows"])
	if len(rows) != 1 {
		t.Fatalf("在线且有 daily 数据的号应出一行: %v", res["rows"])
	}
	row, _ := rows[0].(map[string]any)
	if row["account"] != "sd6@xy3.com" || row["level"] != float64(45) {
		t.Fatalf("行应带账号与等级: %v", row)
	}
	queue := asSlice(row["queue"])
	if len(queue) != 1 {
		t.Fatalf("queue 应有一条进度: %v", row["queue"])
	}
	q0, _ := queue[0].(map[string]any)
	if q0["share_key"] != "share_daily_大唐神捕" || q0["done"] != float64(4) || q0["limit"] != float64(10) {
		t.Fatalf("queue 条目应带 share_key/done/limit/state: %v", q0)
	}
	if q0["kind"] != "shenbu" {
		t.Fatalf("queue 条目应带策略 kind（前端按它映射图标/表头）: %v", q0)
	}
	if row["current"] != "share_daily_大唐神捕" {
		t.Fatalf("意图在神捕 → current 应为玩法键: %v", row["current"])
	}
	if len(asSlice(row["order"])) != 0 {
		t.Fatalf("轮转顺序 P2 再填，现在应为空数组: %v", row["order"])
	}
}

// /api/autotask 的池与任务视图按 kind 自动扩展：shenbu 必须在（前端策略卡直接获得）。
func TestAutoTaskViewsIncludeShenbu(t *testing.T) {
	env := newTestEnv(t, "")
	res := getJSON(t, env.srv.URL+"/api/autotask")
	pools, _ := res["pools"].(map[string]any)
	if _, ok := pools["shenbu"]; !ok {
		t.Fatalf("池视图应含 shenbu（kind 驱动自动获得）: %v", pools)
	}
	tasks := asSlice(res["tasks"])
	found := false
	for _, it := range tasks {
		st, _ := it.(map[string]any)
		if st["kind"] == "shenbu" {
			found = true
			if st["label"] != "大唐神捕" {
				t.Fatalf("shenbu 的中文名应为大唐神捕: %v", st)
			}
		}
	}
	if !found {
		t.Fatalf("任务视图应含 shenbu: %v", res["tasks"])
	}
}

// 候选：抓鬼已满的 45 级号（意图仍是 ghost）应进 shenbu 候选 —— 填补抓鬼满额后的空档。
func TestShenbuCandidateForGhostFullAccount(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com" // 夹具账号：36 级（心跳 45 会覆盖）、该区 usable
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
				"count_date": time.Now().Format("20060102")}}},
		"_zone": testsupportZone()})

	res := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := res["candidates"].(map[string]any)
	if cands["shenbu"] != float64(1) {
		t.Fatalf("抓鬼满额的 45 级号应进大唐神捕候选（填补空档）: %v", cands)
	}
}

// queue 的 state = **队列状态枚举**（lead 裁决 A）：
//
//	满额 → done；有心跳条目（=在跑）且未满 → running；已判该跑但无心跳条目 → pending（合成）。
func TestDailyOverviewStateEnums(t *testing.T) {
	t.Setenv("CTRL_SHARE_DAILY", "1")
	env := newTestEnv(t, "")
	feed := func(acc, phase string, done, limit int) map[string]any {
		return map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0,
			"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": done, "limit": limit, "state": phase}}
	}
	// 两个"在跑"（相位不同不影响队列语义）+ 一个满额；另有一个"未跑"的靠意图登记
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			feed("st_ready@xy3.com", "READY", 0, 10),   // 有心跳条目 → running
			feed("st_submit@xy3.com", "SUBMIT", 3, 10), // 有心跳条目 → running
			feed("st_done@xy3.com", "SUBMIT", 10, 10),  // 满额 → done
			map[string]any{"account": "st_planned@xy3.com", "level": 45, "online": true,
				"state": "IDLE", "task_index": 0}, // 无 daily 条目
		},
		"_zone": testsupportZone()})
	// 手动把"未跑"号登记为神捕意图（模拟"判了该跑但心跳还没报"）
	if _, _, err := env.ev.Intents.Apply("st_planned@xy3.com",
		intent.Decision{Known: true, Kind: intent.KindShenbu, Reason: "测试登记"}, testsupportZone(), "manual"); err != nil {
		t.Fatalf("登记意图失败: %v", err)
	}

	res := getJSON(t, env.srv.URL+"/api/daily/overview")
	want := map[string]struct {
		state  string
		marked bool
	}{
		"st_ready@xy3.com":   {"running", false},
		"st_submit@xy3.com":  {"running", false},
		"st_done@xy3.com":    {"done", false},
		"st_planned@xy3.com": {"pending", true}, // 合成条目
	}
	got := 0
	for _, it := range asSlice(res["rows"]) {
		row, _ := it.(map[string]any)
		acc, _ := row["account"].(string)
		exp, ok := want[acc]
		if !ok {
			continue
		}
		got++
		q := asSlice(row["queue"])
		if len(q) != 1 {
			t.Fatalf("%s 应有一条进度: %v", acc, row["queue"])
		}
		q0, _ := q[0].(map[string]any)
		if q0["state"] != exp.state {
			t.Fatalf("%s 队列状态应为 %s，实际 %v（raw=%v）", acc, exp.state, q0["state"], q0["raw_state"])
		}
		if marked, _ := q0["marked"].(bool); marked != exp.marked {
			t.Fatalf("%s marked 应为 %v: %v", acc, exp.marked, q0)
		}
		// 原始相位只出现在 raw_state（不占 state）
		if acc != "st_planned@xy3.com" && q0["raw_state"] == "" {
			t.Fatalf("%s 应保留机器人原相位到 raw_state: %v", acc, q0)
		}
	}
	if got != len(want) {
		t.Fatalf("应覆盖 %d 个号，实际 %d: %v", len(want), got, res["rows"])
	}
}

// shenbuCandN 当前 shenbu 候选数（多个用例共用）。
func shenbuCandN(t *testing.T, env *testEnv) float64 {
	t.Helper()
	res := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := res["candidates"].(map[string]any)
	n, _ := cands["shenbu"].(float64)
	return n
}

// 满额停止事件（机器人 __request_stop 的 error，带 done/limit）落满额表：
// 心跳可能来不及看到 done≥limit（满额后 ~2s 停、心跳 3s 一跳）；且"满额"是正常收工，
// 不该计入任务错误（否则 ErrRepeat 逐日累加，第 3 天该号被判卡住不再派）。
func TestShareDailyFullMarkedFromStopEvent(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com"
	base := func() map[string]any {
		return map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0,
			"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
				"count_date": time.Now().Format("20060102")}}
	}
	// ① 收尾窗口的最后一条心跳：done=9/10（还没满，仍可进候选）
	rb1 := base()
	rb1["daily"] = map[string]any{"share_key": "share_daily_大唐神捕", "done": 9, "limit": 10, "state": "SUBMIT"}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{rb1}, "_zone": testsupportZone()})
	if n := shenbuCandN(t, env); n != 1 {
		t.Fatalf("未满时应进候选: %v", n)
	}
	// ② 机器人满额停止事件（心跳没抓到 10/10）
	env.ev.HandleEvent(map[string]any{"type": "error", "account": acc,
		"code": "SHARE_DAILY_DAILY_LIMIT", "msg": "今日次数已用完(10/10)", "state": "STOPPED",
		"reason": "今日次数已用完(10/10)", "done": 10, "limit": 10, "_zone": testsupportZone()})
	// ③ 停止后心跳：daily=None（机器人 enabled=false 不再上报）
	rb2 := base()
	rb2["daily"] = nil
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{rb2}, "_zone": testsupportZone()})
	if n := shenbuCandN(t, env); n != 0 {
		t.Fatalf("满额停止事件后应靠满额表继续拦住候选: %v", n)
	}
	if r, _ := env.st.Get(acc); r.ErrCode != "" || r.ErrRepeat != 0 {
		t.Fatalf("满额停止是正常收工，不该计入任务错误: code=%q repeat=%d", r.ErrCode, r.ErrRepeat)
	}
	// 总览补一条 10/10 done（心跳已无 daily）
	ov := getJSON(t, env.srv.URL+"/api/daily/overview")
	found := false
	for _, it := range asSlice(ov["rows"]) {
		row, _ := it.(map[string]any)
		if row["account"] != acc {
			continue
		}
		for _, qi := range asSlice(row["queue"]) {
			q0, _ := qi.(map[string]any)
			if q0["state"] == "done" && q0["done"] == float64(10) && q0["limit"] == float64(10) &&
				q0["marked"] == true {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("总览应为满额号补一条 10/10 done（marked）: %v", ov["rows"])
	}
}

// 满额兜底：机器人满额后 request_stop → 心跳 daily 变 None；中控靠独立满额表继续拦候选，
// 并在总览里补一条 `10/10 done`（否则该玩法在面板上"消失"）。
func TestShareDailyFullMarkedAfterStop(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com"
	base := func(daily any) map[string]any {
		rb := map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0,
			"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
				"count_date": time.Now().Format("20060102")}}
		if daily != nil {
			rb["daily"] = daily
		}
		return rb
	}
	// ① 收尾窗口：done=10/10（此时 enabled 仍 true，心跳带 daily）→ 打标
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{base(map[string]any{"share_key": "share_daily_大唐神捕",
			"done": 10, "limit": 10, "state": "SUBMIT"})},
		"_zone": testsupportZone()})
	// ② 满额收工后：机器人不上报 daily（显式 None，与 client.py 的 st["daily"]=None 同形）
	rb2 := base(nil)
	rb2["daily"] = nil
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{rb2}, "_zone": testsupportZone()})
	if r, _ := env.st.Get(acc); r.Daily != nil {
		t.Fatalf("心跳 daily=None 应清掉运行时 daily（否则下面的兜底断言测的是旧值）: %v", r.Daily)
	}

	// 候选：满额表兜底 → 不该再进（否则会反复派、被 done_limit 拒，还打断已转游荡的号）
	res := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := res["candidates"].(map[string]any)
	if cands["shenbu"] != float64(0) {
		t.Fatalf("满额收工后（心跳无 daily）应靠满额表继续拦住候选: %v", cands)
	}
	// 总览：补一条 done 条目（marked=true）
	ov := getJSON(t, env.srv.URL+"/api/daily/overview")
	found := false
	for _, it := range asSlice(ov["rows"]) {
		row, _ := it.(map[string]any)
		if row["account"] != acc {
			continue
		}
		for _, qi := range asSlice(row["queue"]) {
			q0, _ := qi.(map[string]any)
			if q0["share_key"] != "share_daily_大唐神捕" {
				continue
			}
			found = true
			if q0["state"] != "done" || q0["done"] != float64(10) || q0["limit"] != float64(10) {
				t.Fatalf("满额兜底条目应为 10/10 done: %v", q0)
			}
			if q0["marked"] != true {
				t.Fatalf("兜底条目应标 marked=true（来源=中控满额记忆，不是心跳）: %v", q0)
			}
		}
	}
	if !found {
		t.Fatalf("总览应为满额号补一条神捕条目: %v", ov["rows"])
	}
}

// 开关默认关：≥40 号在「自动分配」仍走抓鬼（旧路径），shenbu 分支不可达。
func TestStartAutoWithoutShareDailyFlagKeepsLegacy(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)      // 抓鬼载荷（旧路径要用）
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir) // 分享日常载荷也就绪 → 更严格（若误判会真发）

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": "sd7@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 0, "limit": 10, "state": "IDLE"}}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"sd7@xy3.com"}}, nil)
	if body["ok"] != true || body["mode"] != "auto" {
		t.Fatalf("应走自动分配: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" {
		t.Fatalf("开关默认关：45 级号（daily 未满）应仍走抓鬼旧路径: %v", cmd)
	}
	groups, _ := body["groups"].(map[string]any)
	if len(asSlice(groups["share_daily_start"])) != 0 {
		t.Fatalf("默认关时不该有号分给 share_daily_start: %v", groups)
	}
}

// 开关开启：45 级 + daily 未满 → 「自动分配」发 share_daily_start（带 share_key/载荷）。
func TestStartAutoWithShareDailyFlagDispatchesShenbu(t *testing.T) {
	t.Setenv("CTRL_SHARE_DAILY", "1")
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": "sd8@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 1, "limit": 10, "state": "IDLE"}}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"sd8@xy3.com"}}, nil)
	if body["ok"] != true {
		t.Fatalf("应下发成功: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_大唐神捕" {
		t.Fatalf("开关开启：满足条件的号应走 share_daily_start: %v", cmd)
	}
	if cmd["daily_limit"] != float64(10) {
		t.Fatalf("应带 daily_limit: %v", cmd)
	}
	groups, _ := body["groups"].(map[string]any)
	if len(asSlice(groups["share_daily_start"])) != 1 {
		t.Fatalf("groups.share_daily_start 应回带该号: %v", groups)
	}
}

// 神捕门槛：策略配置 min_level 覆盖全局（CTRL_SHARE_DAILY_MIN_LEVEL，默认 40）。
func TestShenbuStrategyMinLevelOverridesGlobal(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com"
	feed := func(level int) {
		env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
			"robots": []any{map[string]any{"account": acc, "level": level, "online": true,
				"state": "IDLE", "task_index": 0,
				"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
					"count_date": time.Now().Format("20060102")}}},
			"_zone": testsupportZone()})
	}
	feed(42)
	// 策略门槛 50（覆盖全局 40）→ 42 级不进候选
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, MinLevel: 50, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动神捕池失败: %v", err)
	}
	res := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := res["candidates"].(map[string]any)
	if cands["shenbu"] != float64(0) {
		t.Fatalf("策略门槛 50 应覆盖全局 40（42 级不进候选）: %v", cands)
	}
	// 门槛降到 40 → 进候选（同一号、同一份心跳）
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, MinLevel: 40, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("改门槛失败: %v", err)
	}
	res = getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ = res["candidates"].(map[string]any)
	if cands["shenbu"] != float64(1) {
		t.Fatalf("门槛 40 时 42 级号应进候选: %v", cands)
	}
}

// 神捕门槛：策略未配（0）→ 回落全局 CTRL_SHARE_DAILY_MIN_LEVEL。
func TestShenbuGlobalMinLevelFallback(t *testing.T) {
	t.Setenv("CTRL_SHARE_DAILY_MIN_LEVEL", "45") // 全局抬到 45
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com"
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "level": 42, "online": true,
			"state": "IDLE", "task_index": 0,
			"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
				"count_date": time.Now().Format("20060102")}}},
		"_zone": testsupportZone()})
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, TargetOnline: 5, BatchMin: 1, BatchMax: 1, // MinLevel 未配
	}); err != nil {
		t.Fatalf("启动神捕池失败: %v", err)
	}
	res := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := res["candidates"].(map[string]any)
	if cands["shenbu"] != float64(0) {
		t.Fatalf("策略未配门槛时应用全局 45（42 级不进候选）: %v", cands)
	}
}

// 神捕余额闸：balance_gate>0 时过滤"余额已知且不足"的号；余额未知不拦、0=不启用。
func TestShenbuBalanceGateFiltersPoorAccount(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com"
	feed := func(money any) {
		rb := map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0,
			"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
				"count_date": time.Now().Format("20060102")}}
		if money != nil {
			rb["money"] = money
		}
		env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
			"robots": []any{rb}, "_zone": testsupportZone()})
	}
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, MinLevel: 40, BalanceGate: 1000, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动神捕池失败: %v", err)
	}
	candN := func() float64 {
		res := getJSON(t, env.srv.URL+"/api/autotask")
		cands, _ := res["candidates"].(map[string]any)
		return cands["shenbu"].(float64)
	}
	feed(500) // 余额不足 → 不进候选
	if n := candN(); n != 0 {
		t.Fatalf("余额 500 < 闸值 1000 不该进候选: %v", n)
	}
	feed(5000) // 余额充足 → 进候选
	if n := candN(); n != 1 {
		t.Fatalf("余额 5000 ≥ 闸值 1000 应进候选: %v", n)
	}
	feed(nil) // 余额未知（未上报）→ 不拦
	if n := candN(); n != 1 {
		t.Fatalf("余额未知不该被余额闸拦下: %v", n)
	}
	// 闸值显式设 0（不启用）→ 再喂余额不足也进候选
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, MinLevel: 40, BalanceGate: 0, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}
	feed(100)
	if n := candN(); n != 1 {
		t.Fatalf("余额闸 0=不启用：余额 100 也应进候选: %v", n)
	}
}
