// 2026-09-24：分享日常"今日已派"持久台账 —— 机器人进程重启后心跳 daily 块丢失
// （机器人端模块无状态持久化），「启动 = 恢复当前任务」/候选"不让路"由中控侧台账
// （data/share_daily_assign.json，见 state.MarkShareDailyAssigned）兜底。
//
// 本组用例：
//
//	① 台账续跑：无心跳 + 有台账 + 未满 → 点「启动」仍派 share_daily_start（对照：无台账回落抓鬼）；
//	① 满额让路：台账 + 独立满额表命中 → 不续跑（回落意图）；
//	② 候选：台账号进神捕候选（定时任务自动捡回），无台账让路、满额让路。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

// ① 台账续跑：心跳 daily 丢失（机器人刚重启）但今天派过、未满的号点「启动」仍续跑神捕；
// 对照：同样无心跳但**没台账**的号回落意图（抓鬼）—— 证明是台账在起作用。
func TestStartResumesShareDailyFromLedger(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	ledger, fresh := "sdledger1@xy3.com", "sdledger2@xy3.com"
	feedRobot(t, env, ledger, nil) // 无 daily 块 = 机器人重启后（模块无状态持久化）
	feedRobot(t, env, fresh, nil)
	env.st.MarkShareDailyAssigned(ledger, "share_daily_大唐神捕")

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{ledger, fresh}}, nil)
	if body["ok"] != true || body["mode"] != "auto" {
		t.Fatalf("应走自动分配: %v", body)
	}
	groups, _ := body["groups"].(map[string]any)
	if g := asSlice(groups["share_daily_start"]); len(g) != 1 || g[0] != ledger {
		t.Fatalf("台账命中且未满的号应续跑神捕: %v", groups)
	}
	if g := asSlice(groups["ghost_start"]); len(g) != 1 || g[0] != fresh {
		t.Fatalf("无台账的号应回落抓鬼（对照，证明是台账在起作用）: %v", groups)
	}
	// 实收命令核对（发文顺序：抓鬼先、神捕后）
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" {
		t.Fatalf("第一条应是抓鬼（无台账号）: %v", cmd)
	}
	cmd = rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_大唐神捕" {
		t.Fatalf("第二条应是神捕续跑: %v", cmd)
	}
	if accs := asSlice(cmd["accounts"]); len(accs) != 1 || accs[0] != ledger {
		t.Fatalf("神捕续跑只该带台账号: %v", cmd["accounts"])
	}
	// 分配理由写明续跑（面板可解释）
	reason := ""
	for _, it := range asSlice(body["assignments"]) {
		if m, _ := it.(map[string]any); m["account"] == ledger {
			reason, _ = m["reason"].(string)
		}
	}
	if !strings.Contains(reason, "续跑") {
		t.Fatalf("分配理由应写明续跑（台账）: %q", reason)
	}
}

// ① 满额让路：台账命中但独立满额表也命中（机器人满额收工后 request_stop，心跳不再带
// daily，满额表续记）→ **不续跑**，回落意图（抓鬼）把神捕名额让出来。
func TestStartLedgerFullFallsBackToGhost(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	acc := "sdledger3@xy3.com"
	feedRobot(t, env, acc, nil)
	env.st.MarkShareDailyAssigned(acc, "share_daily_大唐神捕")
	env.st.MarkShareDailyFull(acc, "share_daily_大唐神捕")

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{acc}}, nil)
	if body["ok"] != true {
		t.Fatalf("应接受: %v", body)
	}
	groups, _ := body["groups"].(map[string]any)
	if g := asSlice(groups["share_daily_start"]); len(g) != 0 {
		t.Fatalf("已满的号不该续跑神捕: %v", g)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" {
		t.Fatalf("满额号应回落抓鬼: %v", cmd)
	}
}

// ② 台账号进神捕候选：机器人重启（心跳 daily 丢失）后，定时任务靠台账把"今天派过、未满"
// 的号自动捡回。对照：无台账让路（意图=ghost、抓鬼未满）；满额（独立满额表）让路。
func TestShenbuCandidateFromLedger(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com" // 夹具账号：池内该区可用（心跳等级覆盖池内 36）
	feedRobot(t, env, acc, nil)   // 无 daily 块 = 机器人重启后
	candN := func() float64 {
		res := getJSON(t, env.srv.URL+"/api/autotask")
		cands, _ := res["candidates"].(map[string]any)
		n, _ := cands["shenbu"].(float64)
		return n
	}
	if n := candN(); n != 0 {
		t.Fatalf("无 daily、无台账的 ghost 意图号应让路（对照）: %v", n)
	}
	env.st.MarkShareDailyAssigned(acc, "share_daily_大唐神捕")
	if n := candN(); n != 1 {
		t.Fatalf("台账命中（今天派过、未满）的号应进神捕候选（定时任务捡回）: %v", n)
	}
	env.st.MarkShareDailyFull(acc, "share_daily_大唐神捕")
	if n := candN(); n != 0 {
		t.Fatalf("满额号不该进神捕候选: %v", n)
	}
}
