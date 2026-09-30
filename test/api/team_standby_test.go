// 修3/修4（2026-09-30，组队"不集结"深挖）：
//
//	修3 = /api/team/dispatch 给**在线队员**补发 ghost_start(role=member)（机器人端待命分支唯一触发器）；
//	修4 = /api/team/status 对"台账队员、心跳无队伍态"的号标 need_rejoin（重登丢态可见化）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

func TestTeamDispatchSendsMemberStandby(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	cap, m1, m2 := "ts_cap@xy3.com", "ts_m1@xy3.com", "ts_m2@xy3.com"
	feedRobot(t, env, cap, nil)
	feedRobot(t, env, m1, nil)
	// m2 登记入队但**不喂心跳**（离线）—— 待命指令只应在线的 m1
	env.ev.TeamJobSet(cap, []string{m1, m2}, "invite", "")
	env.ev.TeamMarkReady(cap, []int{})

	_, body := postJSON(t, env.srv.URL+"/api/team/dispatch", map[string]any{"captain": cap}, nil)
	if body["ok"] != true {
		t.Fatalf("就绪后应派发成功: %v", body)
	}

	// ① 队长指令（role=captain）
	c1 := rb.ReadCmd(t, 2*time.Second)
	if c1["cmd"] != "ghost_start" || c1["role"] != "captain" {
		t.Fatalf("第一条应为队长 role=captain: %v", c1)
	}
	// ② 队员待命指令（role=member，只含在线成员 m1）
	c2 := rb.ReadCmd(t, 2*time.Second)
	if c2["cmd"] != "ghost_start" || c2["role"] != "member" {
		t.Fatalf("第二条应为队员 role=member 待命: %v", c2)
	}
	accs := asSlice(c2["accounts"])
	if len(accs) != 1 || accs[0] != m1 {
		t.Fatalf("待命指令应只含在线成员 %s（m2 离线不发），实际 %v", m1, accs)
	}
	if c2["chain"] == nil || c2["chain_id"] != "zhongkui_nav" {
		t.Fatalf("队员载荷应与队长同链: %v", c2["chain_id"])
	}
	// 响应回执：members_sent / members_offline
	ms := asSlice(body["members_sent"])
	mo := asSlice(body["members_offline"])
	if len(ms) != 1 || ms[0] != m1 || len(mo) != 1 || mo[0] != m2 {
		t.Fatalf("回执应带 members_sent=[m1] / members_offline=[m2]: %v", body)
	}
	// 无第三命令
	if c3 := rb.TryReadCmd(300 * time.Millisecond); c3 != nil {
		t.Fatalf("不该有第三条命令: %v", c3)
	}
}

func TestTeamDispatchMemberGates(t *testing.T) {
	// ① 未就绪：队长与队员都零命令
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	cap, m1 := "ts_cap2@xy3.com", "ts_m1b@xy3.com"
	feedRobot(t, env, cap, nil)
	feedRobot(t, env, m1, nil)
	_, body := postJSON(t, env.srv.URL+"/api/team/dispatch", map[string]any{"captain": cap}, nil)
	if body["ok"] != false || !strings.Contains(toStrAny(body["msg"]), "未就绪") {
		t.Fatalf("未就绪应拒绝: %v", body)
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("未就绪不该发任何命令（含队员）: %v", cmd)
	}

	// ② 兼容 ready（心跳 captain+setup_done，台账无成员）：只发队长、无队员指令
	env2 := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env2.cfg.ChainDir)
	rb2 := testsupport.ConnectFakeRobot(t, env2.ctrl)
	defer rb2.Close()
	cap2 := "ts_cap3@xy3.com"
	feedRobot(t, env2, cap2, map[string]any{"team": map[string]any{
		"role": "captain", "parted": false, "setup_done": true}})
	_, body2 := postJSON(t, env2.srv.URL+"/api/team/dispatch", map[string]any{"captain": cap2}, nil)
	if body2["ok"] != true {
		t.Fatalf("兼容路径（心跳 ready）应派发: %v", body2)
	}
	if c := rb2.ReadCmd(t, 2*time.Second); c["role"] != "captain" {
		t.Fatalf("第一条应为队长: %v", c)
	}
	if c2 := rb2.TryReadCmd(300 * time.Millisecond); c2 != nil {
		t.Fatalf("无台账成员时不该发队员指令: %v", c2)
	}
	if !strings.Contains(toStrAny(body2["msg"]), "无台账队员") {
		t.Fatalf("回执应说明无台账队员: %v", body2["msg"])
	}
}

func TestTeamStatusNeedRejoinFlag(t *testing.T) {
	env := newTestEnv(t, "")
	cap, m1 := readyTeam(t, env) // m1 心跳无 team 块（feedRobot 默认）

	body := getJSON(t, env.srv.URL+"/api/team/status")
	robots, _ := body["robots"].(map[string]any)
	m1R, _ := robots[m1].(map[string]any)
	if m1R["need_rejoin"] != true {
		t.Fatalf("台账队员 + 心跳无队伍态 → 应标 need_rejoin: %v", m1R)
	}
	if !strings.Contains(toStrAny(m1R["rejoin_reason"]), "需重新入队") {
		t.Fatalf("应带人话原因: %v", m1R["rejoin_reason"])
	}
	capR, _ := robots[cap].(map[string]any)
	if _, has := capR["need_rejoin"]; has {
		t.Fatalf("队长行不应带 need_rejoin（口径仅队员）: %v", capR)
	}

	// 对照：队员心跳带上队伍态 → need_rejoin 消失
	feedRobot(t, env, m1, map[string]any{"team": map[string]any{
		"role": "member", "parted": false, "setup_done": true}})
	body2 := getJSON(t, env.srv.URL+"/api/team/status")
	robots2, _ := body2["robots"].(map[string]any)
	m1R2, _ := robots2[m1].(map[string]any)
	if _, has := m1R2["need_rejoin"]; has {
		t.Fatalf("恢复队伍态后不该再标 need_rejoin: %v", m1R2)
	}
}
