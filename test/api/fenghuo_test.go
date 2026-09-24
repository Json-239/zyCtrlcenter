// 烽火大唐（fenghuo）中控侧契约（2026-09-24 P1，镜像大唐神捕的全套接线）：
//
//   - 命令：share_daily_start{accounts, share_key="share_daily_宫廷10", chain_id="fenghuo_nav",
//     chain, daily_limit=20, done?} —— 载荷 = 基座 newbie_full + fenghuo_nav 声明；
//   - 载荷缺失 → **硬失败**（一条命令都不发）；
//   - 停策略 → 只给**该玩法**在跑的号下发 share_daily_stop（两个玩法互不误伤）；
//   - 候选/在线数/台账/满额表按玩法键隔离（与神捕互不串）；
//   - 抓鬼让路：烽火池启用且该号当天已派/在跑未满 → 不派 ghost_start；池停用 → 回抓鬼；
//   - 补发（/api/intents/restore）同口径：按意图 kind 带 fenghuo 的 share_key/日限/载荷；
//   - GET /api/daily/overview：share_keys 含两 key，行内 queue 按玩法各一条。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/test/testsupport"
)

// fenghuoDaily 烽火大唐心跳 daily 条目（服务端 share_daily_key=share_daily_宫廷10，日限 20）。
func fenghuoDaily(done, limit int, phase string) map[string]any {
	return map[string]any{"share_key": "share_daily_宫廷10", "done": done, "limit": limit, "state": phase}
}

// 下发 share_daily_start：命令形状 + 载荷完整性（task_order 全 6 个任务号；坐标/网格/路由从基座复用；
// 与神捕的声明文件**不串**）。
func TestLaunchFenghuoCarriesPayload(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir)
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir) // 神捕声明也在 → 用错文件会带错 task_order（更严格）

	ok, msg := env.api.LaunchTask(autotask.KindFenghuo, []string{"fh1@xy3.com"})
	if !ok {
		t.Fatalf("下发烽火大唐应成功: %s", msg)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" {
		t.Fatalf("应收 share_daily_start: %v", cmd)
	}
	if cmd["share_key"] != "share_daily_宫廷10" {
		t.Fatalf("应带玩法键 share_daily_宫廷10（20021.xml 口径）: %v", cmd["share_key"])
	}
	if cmd["chain_id"] != "fenghuo_nav" {
		t.Fatalf("应带 fenghuo_nav 声明文件: %v", cmd["chain_id"])
	}
	if cmd["daily_limit"] != float64(20) {
		t.Fatalf("应带 daily_limit=20（烽火大唐服务端口径）: %v", cmd["daily_limit"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	if len(chain) == 0 {
		t.Fatalf("必须带 chain 载荷（不带机器人拿不到 task_order 与导航）: %v", cmd)
	}
	// R1 防护：task_order 必须列全 6 个任务号（2002101~2002105 + 2002107；2002106 在
	// 20021.xml 中不存在=跳号）。少列会被机器人当「链外任务」静默忽略。
	order := asSlice(chain["task_order"])
	want := map[float64]bool{2002101: false, 2002102: false, 2002103: false,
		2002104: false, 2002105: false, 2002107: false}
	catcherOf := map[float64]string{}
	throwerOf := map[float64]string{}
	for _, it := range order {
		o, _ := it.(map[string]any)
		idx, _ := o["task_index"].(float64)
		if _, known := want[idx]; !known {
			t.Fatalf("task_order 出现未知任务号 %v（应只有 2002101~2002105/2002107）: %v", idx, o)
		}
		want[idx] = true
		if c, _ := o["catcher_npc"].(string); c != "" {
			catcherOf[idx] = c
		}
		if t, _ := o["thrower_npc"].(string); t != "" {
			throwerOf[idx] = t
		}
	}
	for idx, seen := range want {
		if !seen {
			t.Fatalf("task_order 少列任务号 %v（少列会让后续环节被静默忽略）: %v", idx, order)
		}
	}
	// catcher 按 20021.xml 的 task_catcher 填（收尾=秦琼 10149；押送=动态军需官 30029）
	if catcherOf[2002107] != "10149" {
		t.Fatalf("2002107（回复秦琼）的 catcher 应为 10149: %v", catcherOf)
	}
	if catcherOf[2002101] != "30029" {
		t.Fatalf("2002101（押送银两）的 catcher 应为动态军需官 30029: %v", catcherOf)
	}
	// thrower（接取/推进 NPC，**机器人端接口约定**，2026-09-24 对齐）：
	//   全 6 环均为秦琼 10149 —— 2002101~2002105 依据 20021.xml 的 task_thrower（首环 catcher
	//   是动态军需官，显式 thrower 防机器人端把 catcher 误当 broker）；2002107（收尾）XML 的
	//   thrower 无实际接取语义，按 catcher 交互 NPC 显式填 10149（两方一致的接口约定）。
	for _, idx := range []float64{2002101, 2002102, 2002103, 2002104, 2002105, 2002107} {
		if throwerOf[idx] != "10149" {
			t.Fatalf("任务 %v 的 thrower 应为 10149 秦琼（机器人端用它定接取/推进 NPC）: %v", idx, throwerOf)
		}
	}
	// 坐标/网格/路由从基座 newbie_full 复用（专属文件只放声明）
	if npcs, _ := chain["npcs"].(map[string]any); len(npcs) == 0 {
		t.Fatalf("chain.npcs 不能为空（基座链提供）: %v", chain)
	}
	if grids, _ := chain["map_grids"].(map[string]any); len(grids) == 0 {
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
func TestLaunchFenghuoMissingPayloadFailsLoudly(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	// 刻意不装链数据（基座/声明都不存在）

	ok, msg := env.api.LaunchTask(autotask.KindFenghuo, []string{"fh2@xy3.com"})
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

// 停策略 → 只给**该玩法**在跑的号下发 share_daily_stop（神捕的号不误伤）。
func TestAutoTaskStopFenghuoStopsOnlyItsOwn(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir)
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)

	if ok, msg := env.api.LaunchTask(autotask.KindFenghuo, []string{"fh3@xy3.com"}); !ok {
		t.Fatalf("下发烽火大唐应成功: %s", msg)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["cmd"] != "share_daily_start" {
		t.Fatalf("先应收到 share_daily_start: %v", cmd)
	}
	if ok, msg := env.api.LaunchTask(autotask.KindShenbu, []string{"sb3@xy3.com"}); !ok {
		t.Fatalf("下发大唐神捕应成功: %s", msg)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["cmd"] != "share_daily_start" {
		t.Fatalf("神捕也应是 share_daily_start: %v", cmd)
	}

	_, res := postJSON(t, env.srv.URL+"/api/autotask/stop", map[string]any{"kind": "fenghuo"}, nil)
	if res["ok"] != true {
		t.Fatalf("停止 fenghuo 策略应成功: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_stop" {
		t.Fatalf("停策略应下发 share_daily_stop: %v", cmd)
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != "fh3@xy3.com" {
		t.Fatalf("只该带烽火大唐在跑的号（神捕号不误伤）: %v", cmd["accounts"])
	}
}

// 候选 + 在线数：抓鬼满额的 45 级号（心跳带烽火大唐未满）进 fenghuo 候选；
// 池运行时 running 按**该玩法心跳 daily** 计（同抓鬼/孵化口径）。
func TestFenghuoCandidateAndRunningCount(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com" // 夹具账号：36 级（心跳 45 会覆盖）、该区 usable
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
				"count_date": time.Now().Format("20060102")},
			"daily": fenghuoDaily(2, 20, "RUNNING")}},
		"_zone": testsupportZone()})

	res := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := res["candidates"].(map[string]any)
	if cands["fenghuo"] != float64(1) {
		t.Fatalf("抓鬼满额的 45 级号（烽火大唐未满）应进 fenghuo 候选: %v", cands)
	}
	// 池启用后：running 按心跳 daily 在跑计
	if err := env.api.AutoTask.Start(autotask.KindFenghuo, autotask.Config{
		Kind: autotask.KindFenghuo, IntervalSec: 600, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动烽火大唐池失败: %v", err)
	}
	res = getJSON(t, env.srv.URL+"/api/autotask")
	pools, _ := res["pools"].(map[string]any)
	fp, _ := pools["fenghuo"].(map[string]any)
	if fp["running"] != float64(1) || fp["target"] != float64(5) || fp["deficit"] != float64(4) {
		t.Fatalf("fenghuo 池 running/target/deficit 应 = 1/5/4: %v", fp)
	}
}

// 台账/满额表按玩法键隔离：
//   - ① 机器人重启（心跳丢失）+ 抓鬼未满的号靠**本玩法**台账捡回；本玩法满额表拦住；
//   - ② 神捕的台账/满额**不影响** fenghuo 候选（两个玩法各记各的账）。
func TestFenghuoLedgerAndFullIsolation(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.ConnectFakeRobot(t, env.ctrl)
	candF := func() float64 {
		res := getJSON(t, env.srv.URL+"/api/autotask")
		cands, _ := res["candidates"].(map[string]any)
		n, _ := cands["fenghuo"].(float64)
		return n
	}

	// ① 有运行时状态、意图=ghost、抓鬼未满 → 让路（对照：台账没命中就轮不到它）
	acc := "robot0001002@xy3.com"
	feedRobot(t, env, acc, nil) // 45 级、无 daily（机器人重启后）
	if n := candF(); n != 0 {
		t.Fatalf("无 daily、无台账、抓鬼未满的号应让路（对照）: %v", n)
	}
	env.st.MarkShareDailyAssigned(acc, "share_daily_宫廷10")
	if n := candF(); n != 1 {
		t.Fatalf("烽火大唐台账命中（今天派过、未满）应进候选（定时任务捡回）: %v", n)
	}
	env.st.MarkShareDailyFull(acc, "share_daily_宫廷10")
	if n := candF(); n != 0 {
		t.Fatalf("烽火大唐满额表命中应拦住候选: %v", n)
	}

	// ② 无意图/无心跳的池内号（不进让路分支）→ 神捕的账不能影响 fenghuo 候选
	acc2 := "robot0001003@xy3.com"
	addUsableAccount(t, env, acc2, 45)
	if n := candF(); n != 1 {
		t.Fatalf("池内可用、无意图的 45 级号应进 fenghuo 候选（基准）: %v", n)
	}
	// 2026-09-24 二次修正：跨日常让路**不再看另一玩法池启用** —— 号当天已派神捕且未满，
	// fenghuo 就不该抢（两个池互为"另一个日常"，会互相顶掉；判据 = 心跳或台账命中且未满）。
	env.st.MarkShareDailyAssigned(acc2, "share_daily_大唐神捕")
	if n := candF(); n != 0 {
		t.Fatalf("神捕已派未满 → fenghuo 候选应让路（跨日常互斥，2026-09-24 二次修正）: %v", n)
	}
	// 满额 = 自由号 → 让路解除（按玩法键隔离：神捕满额不影响 fenghuo 自己的名额）
	env.st.MarkShareDailyFull(acc2, "share_daily_大唐神捕")
	if n := candF(); n != 1 {
		t.Fatalf("神捕满额 = 自由号 → fenghuo 候选应恢复: %v", n)
	}
	env.st.MarkShareDailyFull(acc2, "share_daily_宫廷10")
	if n := candF(); n != 0 {
		t.Fatalf("烽火大唐自己满额才该拦住候选: %v", n)
	}
}

// 开关默认关 + 无心跳/无台账：45 级号在「自动分配」仍走抓鬼（旧路径），fenghuo 分支不可达。
func TestStartAutoWithoutFenghuoFlagKeepsLegacy(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir) // 声明就绪 → 若误判会真发（更严格）
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": "fh7@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"fh7@xy3.com"}}, nil)
	if body["ok"] != true || body["mode"] != "auto" {
		t.Fatalf("应走自动分配: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" {
		t.Fatalf("开关默认关：45 级号（无日常会话）应仍走抓鬼旧路径: %v", cmd)
	}
	groups, _ := body["groups"].(map[string]any)
	if len(asSlice(groups["share_daily_start"])) != 0 || len(asSlice(groups["share_daily_fenghuo"])) != 0 {
		t.Fatalf("默认关时不该有号分给分享日常: %v", groups)
	}
}

// 开关开启：45 级 + 烽火大唐心跳未满 → 「自动分配」发 fenghuo 的 share_daily_start。
func TestStartAutoWithFenghuoFlagDispatches(t *testing.T) {
	t.Setenv("CTRL_FENGHUO", "1")
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir)
	testsupport.InstallGhostNav(t, env.cfg.ChainDir) // 若误判成抓鬼这里会真发（更严格）

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": "fh8@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"daily": fenghuoDaily(1, 20, "IDLE")}},
		"_zone": testsupportZone()})
	// 事件层判据（intent.Decider.FenghuoEnabled 接线：CTRL_FENGHUO=1 + 未满 → fenghuo 意图）
	if it, ok := env.ev.Intents.Get("fh8@xy3.com"); !ok || it.Kind != intent.KindFenghuo {
		t.Fatalf("45 级 + 烽火大唐未满 + 开关开 应登记 fenghuo 意图: %+v", it)
	}

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"fh8@xy3.com"}}, nil)
	if body["ok"] != true {
		t.Fatalf("应下发成功: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_宫廷10" {
		t.Fatalf("满足条件的号应走烽火大唐 share_daily_start: %v", cmd)
	}
	if cmd["daily_limit"] != float64(20) || cmd["chain_id"] != "fenghuo_nav" {
		t.Fatalf("应带 daily_limit=20 与 fenghuo_nav: %v", cmd)
	}
	groups, _ := body["groups"].(map[string]any)
	if g := asSlice(groups["share_daily_fenghuo"]); len(g) != 1 || g[0] != "fh8@xy3.com" {
		t.Fatalf("groups.share_daily_fenghuo 应回带该号: %v", groups)
	}
}

// 抓鬼让路（2026-09-24 二次修正）：该号当天烽火未满（心跳在跑）→ 不派 ghost_start ——
// **不再要求烽火池启用**（P1 灰度是手动派 3 个号、池未启用；旧口径会误放行 →
// RESTORE/reghost 的 ghost_start 把在跑的烽火顶掉 → 点钟馗卡死 → 熔断，robot0001029 实证）。
// 停用（面板 = 清台账 + 停会话）后 → 回抓鬼（fcf923e 口径）。
func TestGhostYieldsToFenghuo(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com"
	feedRobot(t, env, acc, map[string]any{
		"ghost": map[string]any{"done": 3, "limit": 50, "enabled": false,
			"count_date": time.Now().Format("20060102")},
		"daily": fenghuoDaily(2, 20, "RUNNING"),
	})
	ghostN := func() float64 {
		res := getJSON(t, env.srv.URL+"/api/autotask")
		cands, _ := res["candidates"].(map[string]any)
		n, _ := cands["ghost"].(float64)
		return n
	}
	// 池未启用（灰度手动派发）：心跳在跑未满 → 照让路（新口径；旧口径这里放行 → 顶掉事故）
	if n := ghostN(); n != 0 {
		t.Fatalf("烽火未满（在跑）→ 抓鬼候选应让路（池未启用也一样：保护手动派发的灰度号）: %v", n)
	}
	if err := env.api.AutoTask.Start(autotask.KindFenghuo, autotask.Config{
		Kind: autotask.KindFenghuo, IntervalSec: 600, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动烽火大唐池失败: %v", err)
	}
	if n := ghostN(); n != 0 {
		t.Fatalf("烽火池启用且该号当天未满 → 抓鬼候选应让路: %v", n)
	}
	// 面板停用（HTTP 路径）：清台账 + 在跑号收工；模拟机器人不再上报 daily → 回抓鬼
	if _, res := postJSON(t, env.srv.URL+"/api/autotask/stop", map[string]any{"kind": "fenghuo"}, nil); res["ok"] != true {
		t.Fatalf("停用烽火失败: %v", res)
	}
	feedRobot(t, env, acc, map[string]any{"daily": nil}) // 模拟机器人收到 share_daily_stop：心跳不再带 daily
	if n := ghostN(); n != 1 {
		t.Fatalf("烽火停用（记录作废+停会话）→ 这些号回抓鬼（不让路，否则两头不跑）: %v", n)
	}
}

// 补发（按意图）与「启动」同口径：带 fenghuo 的 share_key/daily_limit/done，多号合并成一条。
func TestIntentsRestoreFenghuoCarriesPayload(t *testing.T) {
	t.Setenv("CTRL_FENGHUO", "1")
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir)
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	if err := env.api.AutoTask.Start(autotask.KindFenghuo, autotask.Config{
		Kind: autotask.KindFenghuo, IntervalSec: 600, TargetOnline: 10, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动烽火大唐池失败: %v", err)
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "fh9@xy3.com", "level": 45, "online": true, "state": "IDLE",
				"task_index": 0, "daily": fenghuoDaily(3, 20, "RUNNING")},
			map[string]any{"account": "fh10@xy3.com", "level": 46, "online": true, "state": "IDLE",
				"task_index": 0, "daily": fenghuoDaily(0, 20, "IDLE")},
		},
		"_zone": testsupportZone()})

	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res["ok"] != true || res["sent"] != float64(2) || res["commands"] != float64(1) {
		t.Fatalf("两个烽火号应合并成 1 条命令（sent=2 / commands=1）: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_宫廷10" {
		t.Fatalf("补发必须是带 fenghuo share_key 的 share_daily_start: %v", cmd)
	}
	if cmd["chain_id"] != "fenghuo_nav" || cmd["daily_limit"] != float64(20) {
		t.Fatalf("补发同口径：带 fenghuo_nav 与 daily_limit=20: %v", cmd)
	}
	chain, _ := cmd["chain"].(map[string]any)
	if len(asSlice(chain["task_order"])) != 6 {
		t.Fatalf("补发也要带完整 task_order（6 个任务号）: %v", chain["task_order"])
	}
	done, _ := cmd["done"].(map[string]any)
	if done["fh9@xy3.com"] != float64(3) {
		t.Fatalf("补发应带各号已做次数（重新下发不丢进度）: %v", done)
	}
	if accs := asSlice(cmd["accounts"]); len(accs) != 2 {
		t.Fatalf("一条命令应覆盖两个账号: %v", cmd["accounts"])
	}
}

// 总览：一个号的心跳同时带两个玩法 → queue 两条（kind 各自正确）、current 取在跑的（shenbu 优先序）。
func TestDailyOverviewTwoDailies(t *testing.T) {
	env := newTestEnv(t, "")
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": "twokey@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"daily": []any{shenbuDaily(1, 10, "RUNNING"), fenghuoDaily(2, 20, "KILL")}}},
		"_zone": testsupportZone()})

	res := getJSON(t, env.srv.URL+"/api/daily/overview")
	rows := asSlice(res["rows"])
	if len(rows) != 1 {
		t.Fatalf("在线且有 daily 数据的号应出一行: %v", res["rows"])
	}
	row, _ := rows[0].(map[string]any)
	q := asSlice(row["queue"])
	if len(q) != 2 {
		t.Fatalf("两个玩法各一条进度: %v", row["queue"])
	}
	k0, _ := q[0].(map[string]any)
	k1, _ := q[1].(map[string]any)
	if k0["kind"] != "shenbu" || k0["share_key"] != "share_daily_大唐神捕" {
		t.Fatalf("第一条应为神捕（固定次序 ghost→newbie→shenbu→fenghuo）: %v", k0)
	}
	if k1["kind"] != "fenghuo" || k1["share_key"] != "share_daily_宫廷10" || k1["limit"] != float64(20) {
		t.Fatalf("第二条应为烽火大唐（含日限 20）: %v", k1)
	}
	if row["current"] != "share_daily_大唐神捕" {
		t.Fatalf("current 应取在跑的玩法（按家族次序取第一条心跳）: %v", row["current"])
	}
}
