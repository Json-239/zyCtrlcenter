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
	"path/filepath"
	"testing"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/roampool"
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

// 池配了目标但**停用**：手动"启动(自动分配)"**不拦**（用户意图优先，2026-09-23 调整），
// 但仍按目标截断（超编不该手动再加）——生产 newbie 池正是 target=30 + disabled，
// 这条口径保证"点启动能派新手链，但不会把池灌爆"。
func TestStartAutoPoolDisabledStillQuotaForManual(t *testing.T) {
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

	robots := []any{}
	accs := []string{}
	for i := 0; i < 3; i++ {
		acc := fmt.Sprintf("off%d@xy3.com", i)
		accs = append(accs, acc)
		robots = append(robots, map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": robots, "_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": accs,
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if n := len(asSlice(groups["ghost_start"])); n != 2 {
		t.Fatalf("池停用不拦手动，但仍按目标 2 截断 → 应派 2 个，实派 %d（msg=%v）", n, body["msg"])
	}
}

// 游荡池的"任务池缺口"只统计**已启用**的池（2026-09-23 口径修正）：
// 停用的池不会要人，把它算成"缺 30"会让超编收敛提前收工（生产 newbie 池 target=30+disabled）。
func TestRoampoolDeficitSkipsDisabledPools(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	// 测试环境默认不装游荡池 keeper：这里装一个（只为读口径，不 Start）
	env.api.Roampool = roampool.New(filepath.Join(t.TempDir(), "roampool.json"), env.api.RoampoolDeps())

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 100, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	// newbie 未启用 → 只算抓鬼：100 - 0 = 100
	if body := getJSON(t, env.srv.URL+"/api/roampool"); body["deficit"] != float64(100) {
		t.Fatalf("newbie 未启用 → 缺口应为 100（只算抓鬼），实际 %v", body["deficit"])
	}
	// 启用 newbie（目标 30）→ 合计 100 + 30 = 130
	if err := env.api.AutoTask.Start(autotask.KindNewbie, autotask.Config{
		Kind: autotask.KindNewbie, TargetOnline: 30, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动新手池失败: %v", err)
	}
	if body := getJSON(t, env.srv.URL+"/api/roampool"); body["deficit"] != float64(130) {
		t.Fatalf("newbie 启用后缺口应为 130（100+30），实际 %v", body["deficit"])
	}
}

// 自动通道（恢复引擎的池闸 GhostSkipFunc）在池停用时**仍然拦**：
// "用户手动点启动" 与 "系统自动补发" 的口径不同。
func TestGhostSkipPoolDisabledBlocksAuto(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 60, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	env.api.AutoTask.Stop(autotask.KindGhost)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "autooff@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})

	skip := env.api.GhostSkipFunc()
	if blocked, _ := skip("ghost", "autooff@xy3.com"); !blocked {
		t.Fatal("自动通道：抓鬼池已停用 → 闸门应拦下补发")
	}
}
