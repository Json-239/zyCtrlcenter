// 2026-09-29 阶段 2（组队 · Go 侧）契约测试：
//
//	① POST /api/team/dispatch：就绪校验（未就绪拒绝）+ 给队长发 ghost_start role=captain
//	   （导航载荷/daily_limit/accounts=仅队长）；队员零命令。
//	② 普通派发避让：LaunchTask（池/补发共用入口）与「启动(自动分配)」遇到队内号一律不发
//	   （队长/队员/待就绪 job 都算队内）；restorer 的 Skip 闸同款。
//	③ GET /api/team/status 扩展：robots 摘要带心跳 team 块 / ledger_role / parted / token_use_ts。
//
// 就绪台账用真实入口构造（env.ev.TeamJobSet + TeamMarkReady = 与 /api/team/setup 同款路径）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// readyTeam 造一个"已就绪"的两号队（cap + m1），返回它们。
func readyTeam(t *testing.T, env *testEnv) (cap, m1 string) {
	t.Helper()
	cap, m1 = "tcap@xy3.com", "tm1@xy3.com"
	feedRobot(t, env, cap, nil)
	feedRobot(t, env, m1, nil)
	env.ev.TeamJobSet(cap, []string{m1}, "invite", "")
	env.ev.TeamMarkReady(cap, []int{})
	return cap, m1
}

func TestTeamDispatchCaptainAfterReady(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	cap, m1 := "tcap@xy3.com", "tm1@xy3.com"
	feedRobot(t, env, cap, nil)
	feedRobot(t, env, m1, nil)

	// ① 未就绪（台账没有）→ 拒绝 + 零命令
	_, body := postJSON(t, env.srv.URL+"/api/team/dispatch", map[string]any{"captain": cap}, nil)
	if body["ok"] != false || !strings.Contains(toStrAny(body["msg"]), "未就绪") {
		t.Fatalf("未就绪应拒绝并提示等 team_ready，实际 %v", body)
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("未就绪不该下发任何命令，实际 %v", cmd)
	}

	// ② 建队就绪 → 派发队长
	env.ev.TeamJobSet(cap, []string{m1}, "invite", "")
	env.ev.TeamMarkReady(cap, []int{})
	_, body = postJSON(t, env.srv.URL+"/api/team/dispatch", map[string]any{"captain": cap}, nil)
	if body["ok"] != true {
		t.Fatalf("就绪后应派发成功: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" {
		t.Fatalf("应收 ghost_start: %v", cmd)
	}
	if cmd["role"] != "captain" {
		t.Fatalf("队长派发 role 必须是 captain（队员待命，绝不 solo）: %v", cmd["role"])
	}
	if cmd["chain_id"] != "zhongkui_nav" || cmd["chain"] == nil {
		t.Fatalf("必须带抓鬼导航载荷: chain_id=%v chain=%v", cmd["chain_id"], cmd["chain"] != nil)
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != cap {
		t.Fatalf("只该派队长 %s（队员不派任务），实际 %v", cap, accs)
	}
	if cmd["daily_limit"] == nil {
		t.Fatalf("应带 daily_limit（与单人同口径）: %v", cmd)
	}
}

func TestLaunchTaskSkipsTeamAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	cap, m1 := readyTeam(t, env)

	// 队员整批 → 拒发
	if ok, msg := env.api.LaunchTask(autotask.KindGhost, []string{m1}); ok ||
		!strings.Contains(msg, "队内号") {
		t.Fatalf("队员应被普通派发剔除（LaunchTask），实际 ok=%v msg=%s", ok, msg)
	}
	// 队长整批 → 拒发（只能走 /api/team/dispatch）
	if ok, msg := env.api.LaunchTask(autotask.KindGhost, []string{cap}); ok ||
		!strings.Contains(msg, "队内号") {
		t.Fatalf("队长也不该被普通派发（走专用通道），实际 ok=%v msg=%s", ok, msg)
	}
	// 混批：非队号 + 队员 → 只发非队号，且 role=solo
	free := "tfree@xy3.com"
	feedRobot(t, env, free, nil)
	if ok, msg := env.api.LaunchTask(autotask.KindGhost, []string{m1, free}); !ok {
		t.Fatalf("混批应至少派非队号，实际 %s", msg)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != free {
		t.Fatalf("只该派非队号 %s，实际 %v", free, accs)
	}
	if cmd["role"] != "solo" {
		t.Fatalf("普通派发 role=solo: %v", cmd["role"])
	}
}

func TestStartAutoSkipsTeamAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	_, m1 := readyTeam(t, env)

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{m1}}, nil)
	if body["ok"] != false {
		t.Fatalf("只有队内号时不该下发成功: %v", body)
	}
	asg, _ := body["assignments"].([]any)
	found := false
	for _, it := range asg {
		m, _ := it.(map[string]any)
		if m["account"] == m1 {
			found = true
			if m["command"] != "" || !strings.Contains(toStrAny(m["reason"]), "队内号") {
				t.Fatalf("队内号应回带 command=\"\" + 队内号原因，实际 %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("assignments 应含队内号一行说明（不静默），实际 %v", asg)
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("启动(自动分配)不该给队内号发命令，实际 %v", cmd)
	}
}

func TestGhostSkipFuncSkipsTeamAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	cap, m1 := readyTeam(t, env)
	_ = cap
	skip, why := env.api.GhostSkipFunc()("ghost", m1)
	if !skip || !strings.Contains(why, "组队中") {
		t.Fatalf("restorer Skip 闸应拦队内号，实际 skip=%v why=%s", skip, why)
	}
	free := "tfree2@xy3.com"
	feedRobot(t, env, free, nil)
	// 非队号不由"组队"闸拦（可能被其它既有闸拦下，如"池未启用"——那是既有行为，不属本用例）
	if _, why := env.api.GhostSkipFunc()("ghost", free); strings.Contains(why, "组队") {
		t.Fatalf("非队号不该被组队闸拦，实际 why=%s", why)
	}
}

func TestTeamStatusRobotsTeamFields(t *testing.T) {
	env := newTestEnv(t, "")
	cap, m1 := readyTeam(t, env)
	// 心跳带 team 块（阶段 2 机器人端契约；含助战令时间戳）
	feedRobot(t, env, cap, map[string]any{"team": map[string]any{
		"role": "captain", "captain": cap, "members": []any{cap, m1},
		"parted": false, "setup_done": true, "token_use_ts": float64(1790000000000)}})
	feedRobot(t, env, m1, map[string]any{"team": map[string]any{
		"role": "member", "captain": cap, "members": []any{cap, m1},
		"parted": true, "setup_done": true}})
	// 助战令链结果 / 队员态：来自事件（机器人端定稿未加心跳 token 字段）
	env.ev.HandleEvent(map[string]any{"type": "team_token", "account": cap,
		"ok": true, "reason": "READY", "count": 9, "reserve": 330000,
		"use_ts": 1790000000000, "use_count": 1})
	env.ev.HandleEvent(map[string]any{"type": "team_member_state", "account": m1,
		"role": "member", "parted": false, "setup_done": true})

	body := getJSON(t, env.srv.URL+"/api/team/status")
	robots, _ := body["robots"].(map[string]any)
	capR, _ := robots[cap].(map[string]any)
	tm, _ := capR["team"].(map[string]any)
	if tm["role"] != "captain" || capR["ledger_role"] != "captain" {
		t.Fatalf("队长行应带心跳 team.role=captain + ledger_role=captain: %v", capR)
	}
	if capR["token_use_ts"] != float64(1790000000000) {
		t.Fatalf("应透传助战令 token_use_ts: %v", capR["token_use_ts"])
	}
	tk, _ := capR["token_last"].(map[string]any)
	if tk["ok"] != true || tk["reason"] != "READY" || tk["count"] != float64(9) {
		t.Fatalf("应带事件来源的助战令状态 token_last: %v", capR["token_last"])
	}
	m1R, _ := robots[m1].(map[string]any)
	tm2, _ := m1R["team"].(map[string]any)
	if tm2["role"] != "member" || m1R["parted"] != true || m1R["ledger_role"] != "member" {
		t.Fatalf("队员行应带 member/parted=true/ledger_role=member: %v", m1R)
	}
	ms, _ := m1R["member_state"].(map[string]any)
	if ms["parted"] != false || ms["setup_done"] != true {
		t.Fatalf("应带事件来源的队员态 member_state: %v", m1R["member_state"])
	}
}

// 心跳 team 块（机器人端定稿：只有 role_id、无账号）→ 台账**缺失**（如中控重启后内存台账为空）
// 也能避让 + status 里队长账号按 rid 反查。
func TestTeamHeartbeatRoleIDResolutionAndAvoidance(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	cap, m1 := "hcap@xy3.com", "hm1@xy3.com"
	feedRobot(t, env, cap, map[string]any{"role_id": 501, "team": map[string]any{
		"role": "captain", "captain_role_id": 501, "member_role_ids": []any{502},
		"parted": false, "setup_done": true, "token_use_ts": float64(1790000000000),
		"token_count": 8}})
	feedRobot(t, env, m1, map[string]any{"role_id": 502, "team": map[string]any{
		"role": "member", "captain_role_id": 501, "member_role_ids": []any{502},
		"parted": false, "setup_done": true}})

	// ① 台账为空（未走 /api/team/setup）→ 心跳兜底避让生效
	if ok, msg := env.api.LaunchTask(autotask.KindGhost, []string{m1}); ok ||
		!strings.Contains(msg, "队内号") {
		t.Fatalf("心跳 team 块（member）应触发避让，实际 ok=%v msg=%s", ok, msg)
	}
	if ok, _ := env.api.LaunchTask(autotask.KindGhost, []string{cap}); ok {
		t.Fatal("心跳 team 块（captain）应触发避让")
	}

	// ② 建队台账 + 最终格式心跳（rid-only）→ status 按 rid 反查队长账号；
	//    心跳-only（无台账）的避让已由 ① 覆盖（status robots 只列台账成员，不重复断言）。
	env.ev.TeamJobSet(cap, []string{m1}, "invite", "")
	env.ev.TeamMarkReady(cap, nil)
	body := getJSON(t, env.srv.URL+"/api/team/status")
	robots, _ := body["robots"].(map[string]any)
	capR, _ := robots[cap].(map[string]any)
	if capR["team_role"] != "captain" || capR["team_captain"] != cap ||
		capR["ledger_role"] != "captain" {
		t.Fatalf("队长行应带心跳角色+rid 反查队长账号+台账角色: %v", capR)
	}
	m1R, _ := robots[m1].(map[string]any)
	if m1R["team_role"] != "member" || m1R["team_captain"] != cap {
		t.Fatalf("队员行 team_captain 应按 rid 反查为队长账号 %s: %v", cap, m1R)
	}
}

// 游荡派发避让（DispatchRoamEx 是"面板下发 + 游荡池 keeper"共用的唯一出口）：
// 队内号一律不发 random_walk，失败原因可见（不静默）。
func TestDispatchRoamSkipsTeamAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	_, m1 := readyTeam(t, env)

	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{m1}, "mapid": 6}, nil)
	if body["ok"] != false || body["sent"] != float64(0) {
		t.Fatalf("队内号不该被派游荡，实际 %v", body)
	}
	if !strings.Contains(toStrAny(body["msg"]), "队内号") {
		t.Fatalf("失败原因应说明队内号避让，实际 %v", body["msg"])
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("不该下发 random_walk，实际 %v", cmd)
	}

	// 混批：非队号照常派游荡（只发非队号）
	free := "tfree3@xy3.com"
	feedRobot(t, env, free, nil)
	_, body = postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{m1, free}, "mapid": 6}, nil)
	if body["ok"] != true || body["sent"] != float64(1) {
		t.Fatalf("混批应只派非队号（sent=1），实际 %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != free {
		t.Fatalf("只该派非队号 %s，实际 %v", free, accs)
	}
}
