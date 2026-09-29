// 2026-09-29 阶段 2（组队）：机器人端两个新事件的入台账断言
// （team-feature-plan 定稿：心跳未加 token 字段 → 观测以事件为准）。
//
//	team_token        {ok,reason:"READY|NO_TOKEN|NO_MONEY|BUY_FAIL",count,reserve,use_ts,use_count}
//	team_member_state {role,parted,setup_done}
package event_test

import (
	"testing"

	"zyctrlcenter/test/testsupport"
)

func TestTeamTokenAndMemberStateRecorded(t *testing.T) {
	h, _, runStore, _ := testsupport.NewTestHandler(t, nil)

	// ① 助战令失败（D13 判据 ok:false）→ 入台账 + warn 日志 + 落运行历史
	h.HandleEvent(map[string]any{"type": "team_token", "account": "tk@x.com",
		"ok": false, "reason": "NO_MONEY", "count": 0, "reserve": 500, "use_ts": 0, "use_count": 0})
	rec := h.TeamTokenLast("tk@x.com")
	if rec == nil || rec["ok"] != false || rec["reason"] != "NO_MONEY" || rec["reserve"] != 500 {
		t.Fatalf("team_token 应入台账（ok/reason/reserve），实际 %v", rec)
	}
	if !testsupport.StoreHasType(runStore, "team_token") {
		t.Fatal("team_token 应落运行历史（原始事件）")
	}

	// ② 助战令就绪（后到事件覆盖前值）→ ok:true
	h.HandleEvent(map[string]any{"type": "team_token", "account": "tk@x.com",
		"ok": true, "reason": "READY", "count": 9, "reserve": 330000,
		"use_ts": 1790000000000.0, "use_count": 1})
	rec = h.TeamTokenLast("tk@x.com")
	if rec == nil || rec["ok"] != true || rec["reason"] != "READY" || rec["count"] != 9 {
		t.Fatalf("后到 team_token 应覆盖台账，实际 %v", rec)
	}

	// ③ 队员态：暂离 → 活跃（覆盖）
	h.HandleEvent(map[string]any{"type": "team_member_state", "account": "mb@x.com",
		"role": "member", "parted": true, "setup_done": true})
	ms := h.TeamMemberStateLast("mb@x.com")
	if ms == nil || ms["parted"] != true || ms["role"] != "member" {
		t.Fatalf("team_member_state（暂离）应入台账，实际 %v", ms)
	}
	h.HandleEvent(map[string]any{"type": "team_member_state", "account": "mb@x.com",
		"role": "member", "parted": false, "setup_done": true})
	ms = h.TeamMemberStateLast("mb@x.com")
	if ms == nil || ms["parted"] != false {
		t.Fatalf("归队事件应覆盖暂离态，实际 %v", ms)
	}

	// ④ 无记录账号 → nil（HTTP 侧不编造）
	if h.TeamTokenLast("nobody@x.com") != nil || h.TeamMemberStateLast("nobody@x.com") != nil {
		t.Fatal("无记录账号应返回 nil")
	}
}
