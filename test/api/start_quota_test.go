// 2026-09-23 P0：`/api/start`(auto) 的**池配额截断**与抓鬼在途记账。
//
// 生产事故（docs/04-测试/分析-20260923-抓鬼分配逻辑.md）：一次"启动(自动分配)"带 231 个号，
// 按意图直派 227 个 ghost_start，把抓鬼会话推到 200+（目标 100）。修复后的口径：
//
//	配额 = max(0, 目标 - 在跑 - 在途)；池停用（Target>0 且 Enabled=false）→ 一个都不派；
//	Target<=0（没配目标）→ 不限（保持旧行为）。
//
// 在途 = 刚下发、机器人还没建立会话的号（TTL 120s），防止"上一批还在路上又放行下一批"。
package api_test

import (
	"fmt"
	"testing"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// 池目标 2 + 5 个候选：第一次只派 2 个；紧接着第二次（这 2 个还在途）一个都不派。
func TestStartAutoGhostPoolQuota(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 60, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}

	accs := []string{}
	robots := []any{}
	for i := 0; i < 5; i++ {
		acc := fmt.Sprintf("quota%d@xy3.com", i)
		accs = append(accs, acc)
		robots = append(robots, map[string]any{
			"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": robots, "_zone": testsupportZone()})

	// 第一次：池目标 2 → 只该派 2 个（其余被配额拦下并回带原因）
	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": accs,
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if got := len(asSlice(groups["ghost_start"])); got != 2 {
		t.Fatalf("池目标 2 → 只该派 2 个，实派 %d（msg=%v）", got, body["msg"])
	}
	// assignments 里被拦下的号必须 command="" 且带原因（面板能看到为什么没派）
	blocked := 0
	for _, v := range asSlice(body["assignments"]) {
		m, _ := v.(map[string]any)
		if m["command"] == "" {
			blocked++
		}
	}
	if blocked != 3 {
		t.Fatalf("应有 3 个被配额/门槛拦下，实得 %d（assignments=%v）", blocked, body["assignments"])
	}

	// 第二次（紧接着）：刚派的 2 个还在途（TTL 内）→ 配额 0 → 一个都不派
	_, body2 := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": accs,
	}, nil)
	groups2, _ := body2["groups"].(map[string]any)
	if n := len(asSlice(groups2["ghost_start"])); n != 0 {
		t.Fatalf("在途已占满配额 → 一个都不该派，实派 %d", n)
	}
}

// 闸门口径：配额已用满（在跑 + 在途 ≥ 目标）→ 恢复引擎的补发闸门必须拦下。
// 旧实现只看"活跃会话数"，刚派发还没建立会话的号看不见 → 连续放行（生产超编根因之一）。
func TestGhostSkipBlockedByPoolQuotaInflight(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 1, IntervalSec: 60, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "q1@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})

	skip := env.api.GhostSkipFunc()
	if blocked, why := skip("ghost", "q1@xy3.com"); blocked {
		t.Fatalf("配额（目标 1，在跑 0，在途 0）未满 → 应放行，实被拦: %s", why)
	}

	// 直派一次 → 该号进入在途（还没建立会话）
	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"q1@xy3.com"},
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if len(asSlice(groups["ghost_start"])) != 1 {
		t.Fatalf("配额 1 → 应派 1 个: %v", body["msg"])
	}
	if blocked, why := skip("ghost", "q1@xy3.com"); !blocked {
		t.Fatalf("该号已在途（占满目标 1）→ 闸门应拦下，实放行（why=%s）", why)
	}
}

// 池配了目标但**停用**：手动"启动"也一个都不派（避免"池停了还被直派拉起"）。
func TestStartAutoGhostPoolDisabledBlocksDispatch(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 60, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	env.api.AutoTask.Stop(autotask.KindGhost) // 停用（保留目标 2）

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "off1@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"off1@xy3.com"},
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if n := len(asSlice(groups["ghost_start"])); n != 0 {
		t.Fatalf("抓鬼池已停用 → 一个都不该派，实派 %d", n)
	}
}
