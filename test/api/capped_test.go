// 卡死熔断（capped）端到端：当日卡死触顶 → 明确状态（restore_capped/cap_until/cap_reason
// + reghost 列表 capped 相位）；restorer/autotask 不再补发/候选（不空转、不占在途）；
// 手动解除（指定账号补发 / POST /api/reghost/resume）后恢复。
//
// 2026-09-23 现场：robot0001009 多次卡死 → 自动重登 3 次 → 只剩一条日志，之后**再无自动
// 恢复动作**，账号一直挂在 ERROR/err_code=STUCK_WAIT_NEXT（面板看不出"已熔断等次日"）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
)

func stuckEvent(acc, code, msg string) map[string]any {
	return map[string]any{"type": "error", "account": acc, "code": code, "msg": msg,
		"_zone": testsupportZone()}
}

// 触顶链路：第 3 次卡死 → 不再下发 robot_manage remove（不再重登）；
// 面板/接口出现明确熔断信息（不再只是 ERROR 挂着、reghost 10 分钟后无痕迹）。
func TestStuckCapAtChurnLimitStopsRelogin(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "cap0001@xy3.com"

	// 第 1、2 次卡死：正常走"下线重登"（每次机器人应收到 robot_manage remove）
	for i := 1; i <= 2; i++ {
		env.ev.HandleEvent(stuckEvent(acc, "STUCK_WAIT_NEXT", "换图推送迟迟未到"))
		cmd := rb.ReadCmd(t, 2*time.Second)
		if cmd["cmd"] != "robot_manage" || cmd["action"] != "remove" {
			t.Fatalf("第 %d 次卡死应下发下线重登: %v", i, cmd)
		}
		// 模拟上一轮重登结束（清在途恢复记录），下一次卡死才能重新登记
		env.api.Reghost.Cancel(acc)
	}

	// 第 3 次卡死：触顶 → 熔断（不再有任何命令）
	env.ev.HandleEvent(stuckEvent(acc, "STUCK_WAIT_NEXT", "换图推送迟迟未到"))
	if extra := rb.TryReadCmd(500 * time.Millisecond); extra != nil {
		t.Fatalf("触顶后不该再下发任何命令（不空转）: %v", extra)
	}

	// 面板：robots[] 带明确熔断字段
	body := getJSON(t, env.srv.URL+"/api/status")
	var row map[string]any
	for _, it := range asSlice(body["robots"]) {
		m, _ := it.(map[string]any)
		if m["account"] == acc {
			row = m
		}
	}
	if row == nil {
		t.Fatalf("状态里应有该号: %v", body["robots"])
	}
	if row["restore_capped"] != true {
		t.Fatalf("应带 restore_capped=true: %v", row)
	}
	if capUntil, _ := row["cap_until"].(string); capUntil == "" {
		t.Fatalf("应带 cap_until（何时自动恢复）: %v", row)
	}
	if capReason, _ := row["cap_reason"].(string); capReason == "" {
		t.Fatalf("应带 cap_reason（为什么熔断）: %v", row)
	}
	if n, _ := row["stuck_count"].(float64); n < 3 {
		t.Fatalf("当日卡死计数应 >=3: %v", row)
	}
	if list := asSlice(body["restore_capped"]); len(list) != 1 {
		t.Fatalf("顶层熔断名单应含该号: %v", body["restore_capped"])
	}

	// /api/autotask 的 reghost 列表：capped 相位（明确"等次日"，不是含糊的 failed）
	at := getJSON(t, env.srv.URL+"/api/autotask")
	found := false
	for _, it := range asSlice(at["reghost"]) {
		m, _ := it.(map[string]any)
		if m["account"] == acc && m["phase"] == "capped" {
			found = true
			if msg, _ := m["last_msg"].(string); !strings.Contains(msg, "熔断") {
				t.Fatalf("capped 文案应说明熔断: %v", m)
			}
			if _, ok := m["cap_until"]; !ok {
				t.Fatalf("capped 条目应带 cap_until: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("reghost 列表应含 capped 条目: %v", at["reghost"])
	}
}

// 熔断号：不进候选、不被自动补发（不静默 —— 回带 capped_skipped）；
// 指定账号补发 = 手动解除 + 补发（用户显式要救它）。
func TestCappedSkipsDispatchAndManualRestoreUncaps(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	// 抓鬼池开起来（restorer 的池状态闸；与生产一致）
	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, IntervalSec: 300, BatchMin: 1, BatchMax: 5, TargetOnline: 10,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}

	acc, other := "cap0002@xy3.com", "cap0003@xy3.com"
	// 两个毕业号（45 级）入池 + 该区可用；acc 触发触顶，other 作对照
	env.pool.Add([]string{acc, other}, "pwd", testZoneAddr, "")
	for _, a := range []string{acc, other} {
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": other, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})

	// 模拟"当日已卡死 3 次"（重复的事件链路径由上面用例覆盖）→ 一次 Request 即触顶熔断
	env.st.Update(acc, func(r *state.Robot) {
		r.StuckDay, r.StuckCount = time.Now().Format("20060102"), 3
	})
	env.api.Reghost.Request(acc, "钟馗对话卡死")
	if !env.st.IsRestoreCapped(acc) {
		t.Fatalf("3 次卡死后应处于熔断: %v", env.st.RestoreCappedList())
	}

	// 1) 候选：只剩对照号（熔断号被排除）
	at := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := at["candidates"].(map[string]any)
	if n, _ := cands["ghost"].(float64); n != 1 {
		t.Fatalf("熔断号不该在候选里（应只剩对照号 1 个）: %v", cands)
	}

	// 2) 全表手动补发：熔断号被拦，且不静默（capped_skipped 回带）
	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res["sent"] != float64(1) {
		t.Fatalf("只该补对照号: %v", res)
	}
	if len(asSlice(res["capped_skipped"])) != 1 {
		t.Fatalf("应回带 capped_skipped（熔断号名单）: %v", res["capped_skipped"])
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	accs := asSlice(cmd["accounts"])
	if cmd["cmd"] != "ghost_start" || len(accs) != 1 || accs[0] != other {
		t.Fatalf("补发的应只有对照号: %v", cmd)
	}

	// 3) 指定熔断号补发 = 手动解除 + 补发（用户显式要救它）
	_, res2 := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{"account": acc}, nil)
	if res2["sent"] != float64(1) {
		t.Fatalf("解除后应补发该号: %v", res2)
	}
	if len(asSlice(res2["uncapped"])) != 1 {
		t.Fatalf("应回带 uncapped: %v", res2["uncapped"])
	}
	if env.st.IsRestoreCapped(acc) {
		t.Fatal("解除后不该再熔断")
	}
	if r, _ := env.st.Get(acc); r.StuckCount != 0 {
		t.Fatalf("解除应清零当日卡死计数（否则调度侧仍按旧计数拦）: %v", r.StuckCount)
	}
	cmd2 := rb.ReadCmd(t, 2*time.Second)
	if cmd2["cmd"] != "ghost_start" {
		t.Fatalf("机器人应收到该号的 ghost_start: %v", cmd2)
	}
	if a2 := asSlice(cmd2["accounts"]); len(a2) != 1 || a2[0] != acc {
		t.Fatalf("补发应指定该号: %v", cmd2["accounts"])
	}
}

// POST /api/reghost/resume：显式解除入口（指定账号 / 全量），未熔断的号原样回带。
func TestRegHostResumeEndpoint(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "cap0004@xy3.com"
	env.st.Update(acc, func(r *state.Robot) { r.Online, r.State, r.Level = true, "IDLE", 45 })

	// 未熔断：指定解除 → count=0 + not_capped
	_, res := postJSON(t, env.srv.URL+"/api/reghost/resume", map[string]any{"account": acc}, nil)
	if res["count"] != float64(0) || len(asSlice(res["not_capped"])) != 1 {
		t.Fatalf("未熔断应回带 not_capped: %v", res)
	}

	// 登记熔断 → 指定解除
	env.st.MarkRestoreCapped(acc, "触顶")
	if !env.st.IsRestoreCapped(acc) {
		t.Fatal("登记后应熔断")
	}
	_, res2 := postJSON(t, env.srv.URL+"/api/reghost/resume", map[string]any{"account": acc}, nil)
	if res2["count"] != float64(1) {
		t.Fatalf("指定解除应成功: %v", res2)
	}
	if env.st.IsRestoreCapped(acc) {
		t.Fatal("解除后不该熔断")
	}
	// 解除后状态接口也不该再带标记
	body := getJSON(t, env.srv.URL+"/api/status")
	for _, it := range asSlice(body["robots"]) {
		m, _ := it.(map[string]any)
		if m["account"] == acc && m["restore_capped"] == true {
			t.Fatalf("解除后不该带 restore_capped: %v", m)
		}
	}

	// 全量解除（不传 account）
	env.st.MarkRestoreCapped("cap0005@xy3.com", "x")
	env.st.MarkRestoreCapped("cap0006@xy3.com", "y")
	_, res3 := postJSON(t, env.srv.URL+"/api/reghost/resume", map[string]any{}, nil)
	if res3["count"] != float64(2) {
		t.Fatalf("全量解除应含两个: %v", res3)
	}
	if n := len(env.st.RestoreCappedList()); n != 0 {
		t.Fatalf("全量解除后名单应为空: %d", n)
	}
}
