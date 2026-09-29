// 2026-09-29 阶段 2（组队）：S2C_TEAM_PROMOTE(90393) 队长转移事件 → 中控台账迁移。
//
// 契约（与 team-feature-plan 对齐）：每个收到 90393 的号各 emit 一条
// {"type":"team_promote","new_captain_role_id":RID[,"new_captain_account":ACC],"new_team_name":NAME}。
// 迁移语义：新队长由队员升为队长，**旧队长转队员**（队伍不散）；rid→账号优先事件自带，
// 否则用心跳 RoleID 反查；台账无匹配队 → 仅记日志（等 team_ready 重建）。
package event_test

import (
	"testing"

	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
)

func TestTeamPromoteMigratesLedger(t *testing.T) {
	h, st, _, _ := testsupport.NewTestHandler(t, nil)
	cap0, m1, m2 := "cap0@x.com", "m1@x.com", "m2@x.com"
	h.TeamJobSet(cap0, []string{m1, m2}, "invite", "")
	h.TeamMarkReady(cap0, []int{})

	// ① 事件带新队长账号（推荐路径）：m1 升队长，旧队长 cap0 转队员，m2 保留
	h.HandleEvent(map[string]any{"type": "team_promote", "account": m1,
		"new_captain_account": m1, "new_team_name": m1 + "的队伍"})
	teams, jobs := h.TeamLedger()
	if len(jobs) != 0 || len(teams) != 1 || teams[0].Captain != m1 {
		t.Fatalf("转移后应只剩 1 队且队长=m1，实际 teams=%+v jobs=%+v", teams, jobs)
	}
	hasCap0, hasM2 := false, false
	for _, m := range teams[0].Members {
		if m == cap0 {
			hasCap0 = true
		}
		if m == m2 {
			hasM2 = true
		}
	}
	if !hasCap0 || !hasM2 {
		t.Fatalf("旧队长应转队员、m2 保留，实际 members=%v", teams[0].Members)
	}
	if role, c, ok := h.TeamRoleOf(cap0); !ok || role != "member" || c != m1 {
		t.Fatalf("旧队长角色应为 member（队长=m1），实际 %v/%v/%v", role, c, ok)
	}
	if role, _, ok := h.TeamRoleOf(m1); !ok || role != "captain" {
		t.Fatalf("m1 应为 captain，实际 %v/%v", role, ok)
	}
	if _, _, ok := h.TeamRoleOf("nobody@x.com"); ok {
		t.Fatal("不在队账号不该命中台账")
	}

	// ② 事件只带 role_id（无账号）→ 心跳 RoleID 反查：m2 升队长
	st.Update(m2, func(r *state.Robot) { r.RoleID = 777; r.Online = true })
	h.HandleEvent(map[string]any{"type": "team_promote", "account": m2, "new_captain_role_id": 777})
	teams, _ = h.TeamLedger()
	if len(teams) != 1 || teams[0].Captain != m2 {
		t.Fatalf("rid 反查后队长应为 m2，实际 %+v", teams)
	}
	hasM1 := false
	for _, m := range teams[0].Members {
		if m == m1 {
			hasM1 = true
		}
	}
	if !hasM1 {
		t.Fatalf("旧队长 m1 应转队员，实际 members=%v", teams[0].Members)
	}

	// ③ 幂等：同一事件重放（各成员各发一条时会重复）→ 不报错、不破坏台账
	h.HandleEvent(map[string]any{"type": "team_promote", "account": m2, "new_captain_role_id": 777})
	teams, _ = h.TeamLedger()
	if len(teams) != 1 || teams[0].Captain != m2 {
		t.Fatalf("重放应幂等，实际 %+v", teams)
	}

	// ④ 找不到队（手工路径）→ 仅日志，不 panic、不改动
	h.HandleEvent(map[string]any{"type": "team_promote", "account": "lonely@x.com",
		"new_captain_account": "lonely@x.com"})
	teams, _ = h.TeamLedger()
	if len(teams) != 1 || teams[0].Captain != m2 {
		t.Fatalf("无匹配队的事件不应改动台账，实际 %+v", teams)
	}
}
