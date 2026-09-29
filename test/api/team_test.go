// 组队（抓鬼试点·阶段 1）：POST /api/team/setup|disband + GET /api/team/status +
// team_* 事件台账（event.Handler 的 team_ready/team_disbanded/… 分支）。
//
// 机器人端契约（2026-09-29 核实于 deploy/single_robot_zy/script/）：
//
//	team_clear(C2S_TEAM_QUIT) → team_setup_member(等邀请) → team_setup_captain(建队+邀请)
//	解散：队长 team_disband + 队员 team_clear 兜底
//
// 命令按 TCP 顺序下发，用例按顺序读取断言。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
)

// setTeamRobot 让账号"在线 + 有 role_id"（invite 模式组队的最低条件）。
func setTeamRobot(env *testEnv, account string, rid int) {
	env.st.Update(account, func(r *state.Robot) {
		r.Online = true
		r.RoleID = rid
		r.State = "READY"
	})
}

// TestTeamSetupInviteDeliversAndLedger 邀请模式全链路：
// 命令顺序（team_clear → team_setup_member → team_setup_captain）+ 台账 pending → ready → 离队更新。
func TestTeamSetupInviteDeliversAndLedger(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	captain, m1, m2 := "robotTeamCap@xy3.com", "robotTeamM1@xy3.com", "robotTeamM2@xy3.com"
	setTeamRobot(env, captain, 101)
	setTeamRobot(env, m1, 201)
	setTeamRobot(env, m2, 202)

	code, body := postJSON(t, env.srv.URL+"/api/team/setup", map[string]any{
		"captain": captain, "members": []string{m1, m2},
	}, nil)
	if code != 200 || body["ok"] != true {
		t.Fatalf("组队下发失败: %d %v", code, body)
	}
	if body["captain_role_id"] != float64(101) || body["mode"] != "invite" {
		t.Fatalf("回执应带 captain_role_id 与 mode: %v", body)
	}

	// 1) team_clear：队长+队员一起清旧队
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "team_clear" {
		t.Fatalf("第 1 条命令应为 team_clear: %v", cmd)
	}
	if got := asSlice(cmd["accounts"]); len(got) != 3 {
		t.Fatalf("team_clear 应带 3 个账号（队长+2 队员）: %v", cmd["accounts"])
	}
	// 2) team_setup_member：只发队员，带 captain_role_id
	cmd = rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "team_setup_member" || cmd["captain_role_id"] != float64(101) ||
		cmd["captain_account"] != captain {
		t.Fatalf("第 2 条命令应为 team_setup_member(队长信息): %v", cmd)
	}
	if got := asSlice(cmd["accounts"]); len(got) != 2 {
		t.Fatalf("team_setup_member 应只带 队员 2 人: %v", cmd["accounts"])
	}
	// 3) team_setup_captain：带 members[{account,role_id}]
	cmd = rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "team_setup_captain" {
		t.Fatalf("第 3 条命令应为 team_setup_captain: %v", cmd)
	}
	members, _ := cmd["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("team_setup_captain 应带 2 名队员: %v", cmd["members"])
	}
	if first, _ := members[0].(map[string]any); first["account"] != m1 || first["role_id"] != float64(201) {
		t.Fatalf("队员载荷应为 {account,role_id}: %v", members[0])
	}

	// 台账：pending
	st := getJSON(t, env.srv.URL+"/api/team/status")
	if st["ok"] != true {
		t.Fatalf("status 应 ok: %v", st)
	}
	jobs, _ := st["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("应有 1 个待就绪任务: %v", st)
	}
	if j, _ := jobs[0].(map[string]any); j["state"] != "pending" || j["captain"] != captain {
		t.Fatalf("job 应为 pending + 队长账号: %v", jobs[0])
	}

	// 队长 team_ready → ready（members 为 role_id 列表）
	env.ev.HandleEvent(map[string]any{"type": "team_ready", "account": captain,
		"role": "captain", "members": []any{201, 202}})
	st = getJSON(t, env.srv.URL+"/api/team/status")
	teams, _ := st["teams"].([]any)
	if len(teams) != 1 {
		t.Fatalf("team_ready 后应有 1 支队伍: %v", st)
	}
	team, _ := teams[0].(map[string]any)
	if team["state"] != "ready" || team["captain"] != captain {
		t.Fatalf("队伍应为 ready: %v", team)
	}
	if rids, _ := team["member_role_ids"].([]any); len(rids) != 2 {
		t.Fatalf("应记录队员 role_id: %v", team["member_role_ids"])
	}
	if jobs, _ = st["jobs"].([]any); len(jobs) != 0 {
		t.Fatalf("ready 后 jobs 应清空: %v", jobs)
	}

	// 队员离队（team_disbanded 来自该队员）→ 从成员列表移除，队长仍在
	env.ev.HandleEvent(map[string]any{"type": "team_disbanded", "account": m1,
		"role_name": "队员一"})
	st = getJSON(t, env.srv.URL+"/api/team/status")
	teams, _ = st["teams"].([]any)
	if len(teams) != 1 {
		t.Fatalf("队员离队不应清掉整队: %v", st)
	}
	team, _ = teams[0].(map[string]any)
	if ms, _ := team["members"].([]any); len(ms) != 1 || ms[0] != m2 {
		t.Fatalf("离队队员应从成员列表移除: %v", team["members"])
	}

	// 队长解散 → 整队清台账
	env.ev.HandleEvent(map[string]any{"type": "team_disbanded", "account": captain})
	st = getJSON(t, env.srv.URL+"/api/team/status")
	if teams, _ = st["teams"].([]any); len(teams) != 0 {
		t.Fatalf("队长解散后台账应清空: %v", st)
	}

	// 其余两个事件不 panic + status 仍正常
	env.ev.HandleEvent(map[string]any{"type": "team_rejoined", "account": m2})
	env.ev.HandleEvent(map[string]any{"type": "team_return_nav", "account": m2,
		"target": []any{24, 1728, 1056}})
	if st = getJSON(t, env.srv.URL+"/api/team/status"); st["ok"] != true {
		t.Fatalf("非台账事件后 status 应正常: %v", st)
	}
}

// TestTeamSetupValidation 参数/在线校验：各类非法输入必须明确拒绝（ok=false + 人话原因）。
func TestTeamSetupValidation(t *testing.T) {
	env := newTestEnv(t, "")
	captain, m1 := "robotTeamCap2@xy3.com", "robotTeamM1x@xy3.com"
	setTeamRobot(env, captain, 111)
	setTeamRobot(env, m1, 211)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"队长必填", map[string]any{"members": []string{m1}}, "captain 必填"},
		{"队员必填", map[string]any{"captain": captain, "members": []string{}}, "members 必填"},
		{"队员不能含队长", map[string]any{"captain": captain, "members": []string{captain}}, "不能包含队长自己"},
		{"队员超上限", map[string]any{"captain": captain,
			"members": []string{"a1", "a2", "a3", "a4", "a5"}}, "上限"},
		{"mode 非法", map[string]any{"captain": captain, "members": []string{m1}, "mode": "x"}, "mode 只支持"},
		{"队员不在线", map[string]any{"captain": captain, "members": []string{"robotMissing@xy3.com"}}, "不在线"},
	}
	for _, c := range cases {
		_, body := postJSON(t, env.srv.URL+"/api/team/setup", c.body, nil)
		if body["ok"] != false {
			t.Fatalf("%s：应拒绝，实际 %v", c.name, body)
		}
		if msg, _ := body["msg"].(string); !strings.Contains(msg, c.want) {
			t.Fatalf("%s：原因应含 %q，实际 %q", c.name, c.want, msg)
		}
	}

	// invite 模式：队员无 role_id → 拒绝（邀请按 role_id 定位）
	env.st.Update(m1, func(r *state.Robot) { r.RoleID = 0 })
	if _, body := postJSON(t, env.srv.URL+"/api/team/setup", map[string]any{
		"captain": captain, "members": []string{m1}}, nil); body["ok"] != false {
		t.Fatalf("队员无 role_id（invite）应拒绝: %v", body)
	}
}

// TestTeamSetupApplyMode apply 模式命令序列：队长先建空队 + 队员申请（不需要队员 role_id）。
func TestTeamSetupApplyMode(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	captain, m1 := "robotTeamCap3@xy3.com", "robotTeamM1y@xy3.com"
	setTeamRobot(env, captain, 121)
	setTeamRobot(env, m1, 0) // apply 模式不要求队员 role_id

	_, body := postJSON(t, env.srv.URL+"/api/team/setup", map[string]any{
		"captain": captain, "members": []string{m1}, "mode": "apply"}, nil)
	if body["ok"] != true {
		t.Fatalf("apply 模式下发失败: %v", body)
	}
	_ = rb.ReadCmd(t, 2*time.Second) // team_clear
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "team_setup_applicant" || cmd["captain_role_id"] != float64(121) {
		t.Fatalf("应为 team_setup_applicant(带 captain_role_id): %v", cmd)
	}
	cmd = rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "team_setup_captain" {
		t.Fatalf("应为 team_setup_captain: %v", cmd)
	}
	if ms, _ := cmd["members"].([]any); len(ms) != 0 {
		t.Fatalf("apply 模式队长建空队（members 为空）: %v", cmd["members"])
	}
}

// TestTeamSetupChannelDown 控制通道未连接：必须失败并给原因（不静默成功）。
func TestTeamSetupChannelDown(t *testing.T) {
	env := newTestEnv(t, "")
	captain, m1 := "robotTeamCap4@xy3.com", "robotTeamM1z@xy3.com"
	setTeamRobot(env, captain, 131)
	setTeamRobot(env, m1, 231)
	_, body := postJSON(t, env.srv.URL+"/api/team/setup", map[string]any{
		"captain": captain, "members": []string{m1}}, nil)
	if body["ok"] != false {
		t.Fatalf("通道未连接应失败: %v", body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "通道未连接") {
		t.Fatalf("原因应说明通道未连接: %q", msg)
	}
}

// TestTeamDisband 解散：命令（队长 disband + 队员 clear）、台账清理、二次解散明确失败。
func TestTeamDisband(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	captain, m1 := "robotTeamCap5@xy3.com", "robotTeamM1w@xy3.com"
	setTeamRobot(env, captain, 141)
	setTeamRobot(env, m1, 241)
	if _, body := postJSON(t, env.srv.URL+"/api/team/setup", map[string]any{
		"captain": captain, "members": []string{m1}}, nil); body["ok"] != true {
		t.Fatalf("组队下发失败: %v", body)
	}
	_ = rb.ReadCmd(t, 2*time.Second) // team_clear
	_ = rb.ReadCmd(t, 2*time.Second) // team_setup_member
	_ = rb.ReadCmd(t, 2*time.Second) // team_setup_captain

	// 不给 accounts 也不给 all → 拒绝（防误解散整队）
	if _, body := postJSON(t, env.srv.URL+"/api/team/disband", map[string]any{}, nil); body["ok"] != false {
		t.Fatalf("空参数解散应拒绝: %v", body)
	}
	// 用队员账号解散：应归属到它所在的队伍
	_, body := postJSON(t, env.srv.URL+"/api/team/disband", map[string]any{
		"accounts": []string{m1}}, nil)
	if body["ok"] != true || body["teams"] != float64(1) {
		t.Fatalf("解散应成功且匹配 1 队: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "team_disband" {
		t.Fatalf("第 1 条应为 team_disband: %v", cmd)
	}
	if got := asSlice(cmd["accounts"]); len(got) != 1 || got[0] != captain {
		t.Fatalf("team_disband 只发队长: %v", cmd["accounts"])
	}
	cmd = rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "team_clear" {
		t.Fatalf("第 2 条应为 team_clear: %v", cmd)
	}

	st := getJSON(t, env.srv.URL+"/api/team/status")
	if teams, _ := st["teams"].([]any); len(teams) != 0 {
		t.Fatalf("解散后台账应为空: %v", st)
	}
	if jobs, _ := st["jobs"].([]any); len(jobs) != 0 {
		t.Fatalf("解散后待就绪任务也应清: %v", st)
	}

	// 二次解散：没有匹配队伍 → 明确失败
	if _, body = postJSON(t, env.srv.URL+"/api/team/disband", map[string]any{
		"accounts": []string{m1}}, nil); body["ok"] != false {
		t.Fatalf("无匹配队伍应失败: %v", body)
	}
}
