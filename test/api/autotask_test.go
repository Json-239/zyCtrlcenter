// 定时自动任务 + 抓鬼等级门槛 + 卡死自动重登恢复（接口层）。
//
// 三个口径在这里钉住：
//  1. /api/autotask* 启停/立即跑/状态可用，且**候选数**按"意图 + 门槛"算；
//  2. 抓鬼等级门槛：<31 级（即使 chain_done）不派抓鬼 —— 手动「启动」与定时任务都不派；
//  3. GHOST_DIALOG_STUCK 事件会登记"待重登恢复"，用户手动取消后清掉。
package api_test

import (
	"strings"
	"testing"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

const testZoneAddr = "127.0.0.1:2300" // testsupport.NewTestZones 的当前区地址

func TestAutoTaskEndpoints(t *testing.T) {
	env := newTestEnv(t, "")

	body := getJSON(t, env.srv.URL+"/api/autotask")
	if body["ok"] != true {
		t.Fatalf("应返回 ok:true: %v", body)
	}
	tasks, _ := body["tasks"].([]any)
	if len(tasks) != 4 {
		t.Fatalf("应有四套独立策略（新手链/抓鬼/孵化/大唐神捕）: %v", body["tasks"])
	}
	if _, ok := body["candidates"].(map[string]any); !ok {
		t.Fatalf("应回带候选数（面板显示「可拉 N 个」）: %v", body)
	}

	// 启动抓鬼策略（间隔 0~300s 随机、每轮 1~3 个）
	_, res := postJSON(t, env.srv.URL+"/api/autotask/start", map[string]any{
		"kind": "ghost", "interval_sec": 60, "jitter_sec": 300, "batch_min": 1, "batch_max": 3,
	}, nil)
	if res["ok"] != true {
		t.Fatalf("启动定时任务应成功: %v", res)
	}
	st, _ := res["state"].(map[string]any)
	if st["enabled"] != true {
		t.Fatalf("状态应显示已启用: %v", st)
	}
	cfg, _ := st["config"].(map[string]any)
	if cfg["jitter_sec"] != float64(300) || cfg["batch_max"] != float64(3) {
		t.Fatalf("参数应回显: %v", cfg)
	}

	// 立即跑一轮（没有候选 → 记一条"没号可拉"的轮次，不算失败）
	_, run := postJSON(t, env.srv.URL+"/api/autotask/run", map[string]any{"kind": "ghost"}, nil)
	if run["ok"] != true {
		t.Fatalf("立即跑一轮应成功: %v", run)
	}

	// 停止
	_, stop := postJSON(t, env.srv.URL+"/api/autotask/stop", map[string]any{"kind": "ghost"}, nil)
	if stop["ok"] != true {
		t.Fatalf("停止应成功: %v", stop)
	}
	if st, _ := stop["state"].(map[string]any); st["enabled"] == true {
		t.Fatalf("停止后 enabled 应为 false: %v", st)
	}

	// 参数错误要明确拒绝
	if _, bad := postJSON(t, env.srv.URL+"/api/autotask/start", map[string]any{"kind": "nope"}, nil); bad["ok"] == true {
		t.Fatalf("未知 kind 应拒绝: %v", bad)
	}
}

// 候选与门槛：新手链只挑未毕业的；抓鬼挑毕业的，但 <31 级（即使 chain_done）被门槛拦下。
func TestAutoTaskCandidatesRespectIntentAndGate(t *testing.T) {
	env := newTestEnv(t, "")
	newbie, ghost, lowDone := "at_newbie@xy3.com", "at_ghost@xy3.com", "at_lowdone@xy3.com"
	env.pool.Add([]string{newbie, ghost, lowDone}, "", testZoneAddr, "")
	for _, a := range []string{newbie, ghost, lowDone} { // 池内记录要"可用"，否则不该被自动拉起
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": newbie, "level": 20, "online": false, "state": "IDLE"},
			map[string]any{"account": ghost, "level": 45, "online": false, "state": "IDLE"},
			// 20 级但链已完成 → 意图是抓鬼，但等级门槛（31）必须拦住它
			map[string]any{"account": lowDone, "level": 20, "chain_done": true, "online": false, "state": "IDLE"},
		},
		"_zone": testsupportZone()})

	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	if cands["newbie"] != float64(1) {
		t.Fatalf("新手链候选应只有 %s（%v）: %v", newbie, newbie, cands)
	}
	if cands["ghost"] != float64(1) {
		t.Fatalf("抓鬼候选应只有 %s（20 级未毕业的号被门槛拦下）: %v", ghost, cands)
	}
}

// 手动「启动」同样受门槛约束：<31 级即使 chain_done 也不派抓鬼，且**说清原因**（不静默）。
func TestStartAutoSkipsLowLevelGhostByGate(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	low := "at_low_done@xy3.com"
	env.pool.Add([]string{low}, "", testZoneAddr, "")
	env.pool.SetZoneState(low, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 20, ChainDone: true})
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": low, "level": 20, "chain_done": true,
			"online": true, "state": "IDLE", "task_index": 0}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{"auto": true, "accounts": []string{low}}, nil)
	groups, _ := body["groups"].(map[string]any)
	if len(asSlice(groups["ghost_start"])) != 0 {
		t.Fatalf("低于门槛的号不该被派抓鬼: %v", body)
	}
	skipped, _ := body["skipped"].([]any)
	if len(skipped) != 1 || !strings.Contains(toStrAny(skipped[0]), "抓鬼门槛") {
		t.Fatalf("应说明为什么跳过（门槛）：%v", body["skipped"])
	}
	if cmd := rb.TryReadCmd(200); cmd != nil {
		t.Fatalf("不该下发任何命令: %v", cmd)
	}
}

// 钟馗对话卡死 → 登记待重登恢复；用户手动取消后清掉（"我停了就不许自动拉起"）。
func TestRegHostTriggeredAndCanceled(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "at_stuck@xy3.com"

	env.ev.HandleEvent(map[string]any{"type": "error", "code": "GHOST_DIALOG_STUCK",
		"account": acc, "msg": "交付捉鬼任务连续 3 次无进展(点钟馗无交付对话)",
		"_zone": testsupportZone()})

	body := getJSON(t, env.srv.URL+"/api/autotask")
	list, _ := body["reghost"].([]any)
	if len(list) != 1 {
		t.Fatalf("卡死事件应登记一条待恢复: %v", body["reghost"])
	}
	st, _ := list[0].(map[string]any)
	if toStrAny(st["account"]) != acc || toStrAny(st["phase"]) == "" {
		t.Fatalf("待恢复项应带账号与阶段: %v", st)
	}
	if !strings.Contains(toStrAny(st["reason"]), "交付") {
		t.Fatalf("应带上机器人给的原因: %v", st)
	}

	_, res := postJSON(t, env.srv.URL+"/api/reghost/cancel", map[string]any{"account": acc}, nil)
	if res["ok"] != true || res["canceled"] != true {
		t.Fatalf("取消应成功: %v", res)
	}
	body2 := getJSON(t, env.srv.URL+"/api/autotask")
	if list2, _ := body2["reghost"].([]any); len(list2) != 0 {
		t.Fatalf("取消后应清空: %v", body2["reghost"])
	}
}

// 号池分区：新手池（未毕业）/ 抓鬼池（已毕业）—— 列表可筛选、每行带 pool、汇总 pool_split；
// /api/autotask 也回带两池"可用数 + 在跑数 + 目标/缺口"。
func TestAccountsPoolSplit(t *testing.T) {
	env := newTestEnv(t, "")
	newbie, ghost, unknown := "ps_new@xy3.com", "ps_ghost@xy3.com", "ps_unknown@xy3.com"
	env.pool.Add([]string{newbie, ghost, unknown}, "", testZoneAddr, "")
	env.pool.SetZoneState(newbie, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 20})
	env.pool.SetZoneState(ghost, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	// unknown 不给任何状态：等级未知 → 未知池
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": newbie, "level": 20, "online": true, "state": "IDLE"},
			map[string]any{"account": ghost, "level": 45, "online": true, "state": "IDLE"},
		},
		"_zone": testsupportZone()})

	// 全部：应带 pool 字段与 pool_split 汇总
	all := getJSON(t, env.srv.URL+"/api/accounts?keyword=ps_")
	split, _ := all["pool_split"].(map[string]any)
	if split["newbie"] != float64(1) || split["ghost"] != float64(1) {
		t.Fatalf("分区汇总应各 1 个（另有 unknown 不计入）: %v", all["pool_split"])
	}
	found := map[string]string{}
	for _, it := range asSlice(all["accounts"]) {
		m, _ := it.(map[string]any)
		found[toStrAny(m["name"])] = toStrAny(m["pool"])
	}
	if found[newbie] != "newbie" || found[ghost] != "ghost" {
		t.Fatalf("每行应带号池分区: %v", found)
	}

	// 只看新手池 / 只看抓鬼池
	nb := getJSON(t, env.srv.URL+"/api/accounts?pool=newbie")
	if n := len(asSlice(nb["accounts"])); n != 1 {
		t.Fatalf("新手池应只剩 1 个（实际 %d）: %v", n, nb["accounts"])
	}
	if toStrAny(nb["pool_filter"]) != "newbie" {
		t.Fatalf("应回带筛选口径: %v", nb["pool_filter"])
	}
	gh := getJSON(t, env.srv.URL+"/api/accounts?pool=ghost")
	for _, it := range asSlice(gh["accounts"]) {
		m, _ := it.(map[string]any)
		if toStrAny(m["pool"]) != "ghost" {
			t.Fatalf("抓鬼池筛选结果混进了别的池: %v", m)
		}
	}

	// /api/autotask 的两池统计
	at := getJSON(t, env.srv.URL+"/api/autotask")
	pools, _ := at["pools"].(map[string]any)
	gp, _ := pools["ghost"].(map[string]any)
	np, _ := pools["newbie"].(map[string]any)
	if gp == nil || np == nil {
		t.Fatalf("应回带两池统计: %v", at["pools"])
	}
	if gp["usable"] == nil || gp["running"] == nil {
		t.Fatalf("池统计要含可用数与在跑数: %v", gp)
	}

	// 目标数在启动策略后回显（保持在线 50）
	if _, st := postJSON(t, env.srv.URL+"/api/autotask/start",
		map[string]any{"kind": "ghost", "target_online": 50, "interval_sec": 300}, nil); st["ok"] != true {
		t.Fatalf("启动应成功: %v", st)
	}
	at2 := getJSON(t, env.srv.URL+"/api/autotask")
	for _, it := range asSlice(at2["tasks"]) {
		m, _ := it.(map[string]any)
		if toStrAny(m["kind"]) != "ghost" {
			continue
		}
		cfg, _ := m["config"].(map[string]any)
		if cfg["target_online"] != float64(50) {
			t.Fatalf("应回显保持在线数: %v", cfg)
		}
		if m["target"] != float64(50) {
			t.Fatalf("状态应带目标数: %v", m)
		}
	}
}

// 新号的"可拉起"链路：**已验证可用但等级未知**的号（刚注册/刚回收）算新手池，
// 也能被新手链策略拉起（上线跑一次机器人就报等级，之后按真实等级归位）。
func TestUnknownLevelUsableGoesToNewbiePool(t *testing.T) {
	env := newTestEnv(t, "")
	fresh := "pn_fresh@xy3.com"
	env.pool.Add([]string{fresh}, "", testZoneAddr, "")
	env.pool.SetZoneState(fresh, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true}) // 等级 0

	// 1) 分区：可用 + 等级未知 → 新手池（不是"未知"）
	body := getJSON(t, env.srv.URL+"/api/accounts?keyword=pn_fresh")
	split, _ := body["pool_split"].(map[string]any)
	if split["newbie"] != float64(1) {
		t.Fatalf("已验证可用的新号应算新手池: %v", body["pool_split"])
	}

	// 2) 新手链候选：应能挑到它（否则"注册了也拉不起来"）
	at := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := at["candidates"].(map[string]any)
	if cands["newbie"] != float64(1) {
		t.Fatalf("新手链候选应包含这个新号: %v", at["candidates"])
	}
}

// ① err_code 残留清理：机器人上报的错误恢复后（心跳不再带 err_code、也不在 ERROR 态），
// 中控必须清掉，否则面板一直显示"需要处理"、熔断计数也一直挂着。
// ② TASK_STUCK（"换图推送迟迟未到, 停链等待处理"）也应进自动重登恢复。
func TestErrCodeClearedAndTaskStuckRecovers(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "st_stuck@xy3.com"

	// 先来一次错误
	env.ev.HandleEvent(map[string]any{"type": "error", "code": "TASK_STUCK", "account": acc,
		"msg": "任务 0 换图推送迟迟未到, 无法走向跳转点, 停链等待处理", "_zone": testsupportZone()})
	if r, ok := env.st.Get(acc); !ok || r.ErrCode != "TASK_STUCK" || r.ErrRepeat < 1 {
		t.Fatalf("错误应记账: %+v", r)
	}
	// TASK_STUCK 也要进自动恢复（下线→重登→补发）
	body := getJSON(t, env.srv.URL+"/api/autotask")
	if list, _ := body["reghost"].([]any); len(list) != 1 {
		t.Fatalf("TASK_STUCK 应登记待恢复: %v", body["reghost"])
	}

	// 机器人恢复正常：心跳不带 err_code、状态不是 ERROR → 残留必须清掉
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "state": "WAIT_GHOST", "online": true, "level": 35}},
		"_zone": testsupportZone()})
	if r, _ := env.st.Get(acc); r.ErrCode != "" || r.ErrRepeat != 0 {
		t.Fatalf("机器人已恢复，残留错误该清掉: err_code=%q repeat=%d", r.ErrCode, r.ErrRepeat)
	}
}

// 反复卡死的号（同日 GHOST_DIALOG_STUCK / TASK_STUCK ≥3 次）当天不再被自动任务拉起 —— 防 churn。
func TestStuckChurnGuard(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "st_churn@xy3.com"
	env.pool.Add([]string{acc}, "", testZoneAddr, "")
	env.pool.SetZoneState(acc, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "level": 45, "online": false, "state": "IDLE"}},
		"_zone": testsupportZone()})

	// 卡死 1 次：仍可被拉起（学习门槛/重登恢复是正常的）
	env.ev.HandleEvent(map[string]any{"type": "error", "code": "GHOST_DIALOG_STUCK", "account": acc,
		"msg": "点钟馗无对话", "_zone": testsupportZone()})
	at := getJSON(t, env.srv.URL+"/api/autotask")
	if cands, _ := at["candidates"].(map[string]any); cands["ghost"] != float64(1) {
		t.Fatalf("卡死 1 次仍应在候选里（等重登恢复后再试）: %v", cands)
	}

	// 同日累计到 3 次：当天不再派
	for i := 0; i < 2; i++ {
		env.ev.HandleEvent(map[string]any{"type": "error", "code": "TASK_STUCK", "account": acc,
			"msg": "换图推送迟迟未到", "_zone": testsupportZone()})
	}
	at = getJSON(t, env.srv.URL+"/api/autotask")
	if cands, _ := at["candidates"].(map[string]any); cands["ghost"] != float64(0) {
		t.Fatalf("同日卡死 3 次的号当天不该再被拉起: %v", cands)
	}
	if r, ok := env.st.Get(acc); !ok || r.StuckCount < 3 || r.StuckDay == "" {
		t.Fatalf("应记录当日卡死次数与日期: %+v", r)
	}
}

// 抓鬼候选过滤"未毕业 + 等级未知/过低"（2026-09-21 现场 robot0009905：1 级真新手号
// 上线瞬间被当抓鬼候选下发，服务端以通知码 71（等级不足，要求 ≥13 级）拒绝，号白跑一趟）。
//
// 旧判据 `!chainDone && level > 0 && level < 31` 让 level=0 穿过过滤，ghostGate 又不拦 0。
// 新口径：未毕业（新手链未完成）一律不派抓鬼（**含等级未知**）；已毕业但等级未知仍允许
// （服务端 gate / 学到的 required_level 兜底，别误伤重连中的号）。
func TestGhostCandidatesSkipNotDoneUnknownOrLowLevel(t *testing.T) {
	cases := []struct {
		name      string
		level     int
		chainDone bool
		want      int
	}{
		{"未毕业+等级未知(0)", 0, false, 0},
		{"未毕业+1 级(低于门槛)", 1, false, 0},
		{"已毕业+等级未知(0)", 0, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "")
			acc := "gc_case@xy3.com"
			env.pool.Add([]string{acc}, "", testZoneAddr, "")
			env.pool.SetZoneState(acc, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true})
			env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
				"robots": []any{map[string]any{"account": acc, "level": tc.level,
					"chain_done": tc.chainDone, "online": true, "state": "IDLE"}},
				"_zone": testsupportZone()})

			body := getJSON(t, env.srv.URL+"/api/autotask")
			cands, _ := body["candidates"].(map[string]any)
			if cands["ghost"] != float64(tc.want) {
				t.Fatalf("抓鬼候选应为 %d 个，实得 %v（%s）: %v", tc.want, cands["ghost"], tc.name, cands)
			}
		})
	}
}

// 保持数口径（2026-09-21 验收缺口 G4）：抓鬼池 running 只算**活跃抓鬼会话**
// （ghost.enabled=true），字段存在但已停（enabled=false，DONE/IDLE 残留）的不算在跑 ——
// 否则 running 虚高（现场 43 个停摆号被算成在跑）、保持数永久"已达标"不补号。
// 与恢复引擎 needsRestore、前端「👻抓鬼」筛选同一口径（state.Robot.GhostActive）。
func TestAutoTaskGhostRunningCountsOnlyActiveSessions(t *testing.T) {
	env := newTestEnv(t, "")
	active, stopped := "at_run_active@xy3.com", "at_run_stopped@xy3.com"
	env.pool.Add([]string{active, stopped}, "", testZoneAddr, "")
	for _, a := range []string{active, stopped} { // 两号都"已验证可用"，都该进抓鬼池
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": active, "level": 45, "online": true, "state": "NAV",
				"ghost": map[string]any{"enabled": true, "state": "RUNNING"}},
			// 停摆形态：字段还在但会话已停（现场 43 个：27 DONE / 15 DIALOG / 1 IDLE）
			map[string]any{"account": stopped, "level": 45, "online": true, "state": "DIALOG",
				"ghost": map[string]any{"enabled": false, "state": "IDLE"}},
		},
		"_zone": testsupportZone()})

	body := getJSON(t, env.srv.URL+"/api/autotask")
	pools, _ := body["pools"].(map[string]any)
	gp, _ := pools["ghost"].(map[string]any)
	if gp == nil {
		t.Fatalf("应回带抓鬼池统计: %v", body["pools"])
	}
	if gp["running"] != float64(1) {
		t.Fatalf("running 只该算活跃抓鬼会话（enabled=true）的 1 个号，实得 %v: %v", gp["running"], gp)
	}
	if gp["usable"] != float64(2) {
		t.Fatalf("两个号都已验证可用，usable 应为 2: %v", gp)
	}
}

// 2026-09-23（前端 26f4660 同口径；Go 侧 isTasking 改为直接复用 waterline.Busy）：
// 「推进中 / 异常」的号不能混进抓鬼候选 ——
//   - SUBMIT（交付/提交中）= 推进中（与 FIGHT/NAV 并列），不算空闲、不派活；
//   - ERROR（机器人上报的卡住/停链态）= 异常，先人工处理，同样不派活；
//   - 对照：IDLE 号仍应正常进候选（证明是"按状态排除"，不是整体失效）。
func TestGhostCandidatesSkipSubmitAndError(t *testing.T) {
	env := newTestEnv(t, "")
	idle, submit, stuck := "cst_idle@xy3.com", "cst_submit@xy3.com", "cst_error@xy3.com"
	env.pool.Add([]string{idle, submit, stuck}, "pwd", testZoneAddr, "")
	for _, a := range []string{idle, submit, stuck} { // 三号都已毕业（45 级）+ 该区可用
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": idle, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": submit, "level": 45, "online": true, "state": "SUBMIT", "task_index": 0},
			map[string]any{"account": stuck, "level": 45, "online": true, "state": "ERROR", "task_index": 0},
		}, "_zone": testsupportZone()})

	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	if cands["ghost"] != float64(1) {
		t.Fatalf("抓鬼候选只该剩 IDLE 号（SUBMIT 推进中 / ERROR 异常都不派活），实得 %v", cands)
	}
}
