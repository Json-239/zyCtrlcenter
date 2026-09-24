// 2026-09-24：抓鬼 / 神捕互斥让路 —— 现场 robot0005274：12:13:06 神捕策略下发
// share_daily_start ok，12 秒后抓鬼策略又下发 ghost_start ok（机器人端 ghost_start
// 与神捕会话互斥，把当天神捕顶掉）；抓鬼池在补位窗口（在跑<目标）会持续派发 →
// 反复杀掉当天的神捕号。
//
// 本组用例钉住两处让路（判据 = 当天已派/在跑神捕且未满：心跳 daily 条目命中**或**
// 中控持久台账命中，见 shareDailyInFlightToday；满额 = 自由号，不拦）：
//
//	① 抓鬼候选（autotaskCandidatesCfg）：台账命中未满 → 不进 ghosts 候选；满额 → 进；
//	   无台账 → 进（对照）；心跳命中同理。
//	② 恢复引擎闸（GhostSkipFunc）：台账/心跳命中未满 → skip + 原因；满额/无台账 → 不 skip。
//
// 2026-09-24 缺口修复（本文件同时钉住）：让路**仅在神捕池启用（enabled）时生效** ——
// 池停用后这些号既不会被神捕池自动派、再拦抓鬼就是"两头不跑"空烧到跨日；口径：池停用
// = 回抓鬼（台账保留，重新启用后候选照旧能捡回）。池启用状态读 Runner.EnabledNoLock
// （无锁，Start/Stop 的 atomic 镜像；持锁回调里可安全调用）。
package api_test

import (
	"strings"
	"testing"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// 玩法键（与配置默认 CTRL_SHARE_DAILY_KEY 一致）。
const ghostMutexShareKey = "share_daily_大唐神捕"

// startShenbuPool 启用大唐神捕策略池 —— 让路的前置条件（池停用 = 当天神捕号回抓鬼）。
func startShenbuPool(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, TargetOnline: 10, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动神捕池失败: %v", err)
	}
}

func ghostCandCount(t *testing.T, env *testEnv) float64 {
	t.Helper()
	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	n, _ := cands["ghost"].(float64)
	return n
}

func shenbuCandCount(t *testing.T, env *testEnv) float64 {
	t.Helper()
	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	n, _ := cands["shenbu"].(float64)
	return n
}

// ① 台账命中且未满 → 抓鬼候选让路（不让派发/补位）；满额 = 自由号 → 回归抓鬼候选；
// 无台账的对照号始终在候选里（证明是台账在起作用，不是整体失效）。
func TestGhostCandidateYieldsToAssignedShenbu(t *testing.T) {
	env := newTestEnv(t, "")
	acc, ctl := "gsm_assigned@xy3.com", "gsm_free@xy3.com"
	env.pool.Add([]string{acc, ctl}, "", testZoneAddr, "")
	for _, a := range []string{acc, ctl} { // 两号都毕业（45 级）+ 该区可用 → 基线都能进抓鬼候选
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	}
	feedRobot(t, env, acc, nil)
	feedRobot(t, env, ctl, nil)
	startShenbuPool(t, env) // 让路前置：神捕池启用（池停用 = 回抓鬼，另有用例）

	if n := ghostCandCount(t, env); n != 2 {
		t.Fatalf("基线：两号都应进抓鬼候选，实得 %v", n)
	}
	// 当天已派神捕（持久台账；机器人重启后心跳丢失的场景）且未满 → 抓鬼让路
	env.st.MarkShareDailyAssigned(acc, ghostMutexShareKey)
	if n := ghostCandCount(t, env); n != 1 {
		t.Fatalf("台账命中且未满 → 该号不该进抓鬼候选（让路），实得 %v", n)
	}
	if n := shenbuCandCount(t, env); n != 1 {
		t.Fatalf("让路后该号应进神捕候选（定时任务捡回续跑），实得 %v", n)
	}
	// 满额（独立满额表）→ 自由号，回归抓鬼候选
	env.st.MarkShareDailyFull(acc, ghostMutexShareKey)
	if n := ghostCandCount(t, env); n != 2 {
		t.Fatalf("神捕满额 = 自由号 → 该号应回归抓鬼候选，实得 %v", n)
	}
}

// ① 心跳 daily 命中（未满）→ 抓鬼候选让路；心跳满额 → 回归（机器人上报 done≥limit
// 的那一刻独立满额表也会打标，两条判据一致）。
func TestGhostCandidateYieldsToDailyHeartbeat(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "gsm_daily@xy3.com"
	env.pool.Add([]string{acc}, "", testZoneAddr, "")
	env.pool.SetZoneState(acc, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	feedRobot(t, env, acc, map[string]any{"daily": map[string]any{
		"share_key": ghostMutexShareKey, "done": 1, "limit": 10, "state": "RUNNING"}})
	startShenbuPool(t, env) // 让路前置：神捕池启用

	if n := ghostCandCount(t, env); n != 0 {
		t.Fatalf("心跳报「在跑神捕未满」→ 不该进抓鬼候选，实得 %v", n)
	}
	// 满额心跳（done=10/10）→ 自由号，回归抓鬼候选
	feedRobot(t, env, acc, map[string]any{"daily": map[string]any{
		"share_key": ghostMutexShareKey, "done": 10, "limit": 10, "state": "STOPPED"}})
	if n := ghostCandCount(t, env); n != 1 {
		t.Fatalf("神捕满额 → 该号应回归抓鬼候选，实得 %v", n)
	}
}

// ② 恢复引擎闸（GhostSkipFunc，restorer 补发前最后一道闸；restorer.TickForce → Skip）：
// 台账命中且未满 → skip + 原因；满额 / 无台账 → 不 skip（对照）。
func TestGhostSkipFuncYieldsToShenbu(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	startShenbuPool(t, env) // 让路前置：神捕池启用
	acc := "gsm_skip@xy3.com"
	feedRobot(t, env, acc, nil)

	skip := env.api.GhostSkipFunc()
	if blocked, why := skip("ghost", acc); blocked {
		t.Fatalf("基线（无神捕记录）应放行补发，实被拦: %s", why)
	}
	env.st.MarkShareDailyAssigned(acc, ghostMutexShareKey)
	if blocked, why := skip("ghost", acc); !blocked || !strings.Contains(why, "神捕") {
		t.Fatalf("台账命中未满 → 应拦下补发并说明原因，实得 blocked=%v why=%q", blocked, why)
	}
	env.st.MarkShareDailyFull(acc, ghostMutexShareKey)
	if blocked, why := skip("ghost", acc); blocked {
		t.Fatalf("神捕满额 = 自由号 → 不该拦补发，实被拦: %s", why)
	}
}

// ② 心跳命中（未满）同样拦补发（机器人正在跑神捕，恢复引擎不许抢）。
func TestGhostSkipFuncYieldsToDailyHeartbeat(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	startShenbuPool(t, env) // 让路前置：神捕池启用
	acc := "gsm_skip_daily@xy3.com"
	feedRobot(t, env, acc, map[string]any{"daily": map[string]any{
		"share_key": ghostMutexShareKey, "done": 3, "limit": 10, "state": "RUNNING"}})

	if blocked, why := env.api.GhostSkipFunc()("ghost", acc); !blocked || !strings.Contains(why, "神捕") {
		t.Fatalf("心跳报「在跑神捕未满」→ 应拦下补发，实得 blocked=%v why=%q", blocked, why)
	}
}

// 2026-09-24 缺口修复：让路**仅在神捕池启用时生效** —— 池停用（enabled=false）的号
// 既不会被神捕池自动派、再拦抓鬼就是"两头不跑"空烧到跨日；口径：池停用 = 回抓鬼。
// 同一组信号（台账 / 心跳）走三态：池启用拦 → Stop 放行 → 重新 Start 恢复拦。
func TestGhostYieldOnlyWhileShenbuPoolEnabled(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	acc, hb := "gsm_pool_acc@xy3.com", "gsm_pool_hb@xy3.com"
	env.pool.Add([]string{acc, hb}, "", testZoneAddr, "")
	for _, a := range []string{acc, hb} {
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	}
	env.st.MarkShareDailyAssigned(acc, ghostMutexShareKey) // 信号①：持久台账（重启后心跳丢失）
	feedRobot(t, env, acc, nil)
	feedRobot(t, env, hb, map[string]any{"daily": map[string]any{ // 信号②：心跳 daily 在跑
		"share_key": ghostMutexShareKey, "done": 1, "limit": 10, "state": "RUNNING"}})

	skip := env.api.GhostSkipFunc()

	// ① 池启用：两种信号都拦（7e12197 的让路回归，对照 Tracks 池停用分支）。
	startShenbuPool(t, env)
	for _, a := range []string{acc, hb} {
		if blocked, why := skip("ghost", a); !blocked || !strings.Contains(why, "神捕") {
			t.Fatalf("池启用：%s 应被拦下补发，实得 blocked=%v why=%q", a, blocked, why)
		}
	}
	if n := ghostCandCount(t, env); n != 0 {
		t.Fatalf("池启用：两号都应让路，实得抓鬼候选 %v", n)
	}

	// ② 池停用：同信号全部放行（回抓鬼，不再两头不跑）。
	env.api.AutoTask.Stop(autotask.KindShenbu)
	for _, a := range []string{acc, hb} {
		if blocked, why := skip("ghost", a); blocked {
			t.Fatalf("池停用：%s 应放行回抓鬼，实被拦: %s", a, why)
		}
	}
	if n := ghostCandCount(t, env); n != 2 {
		t.Fatalf("池停用：两号都应回归抓鬼候选，实得 %v", n)
	}
	if !env.st.ShareDailyAssignedToday(acc, ghostMutexShareKey) {
		t.Fatal("池停用不该清台账（重新启用后候选要能按台账捡回）")
	}
	// 对照：shenbu 自身的补发闸不受影响 —— 池停用时不自动补发神捕（既有口径）。
	if blocked, why := skip("shenbu", acc); !blocked || !strings.Contains(why, "未启用") {
		t.Fatalf("池停用：shenbu 补发应仍被拦（池未启用），实得 blocked=%v why=%q", blocked, why)
	}

	// ③ 重新启用：让路恢复，台账照旧把该号捡回神捕候选。
	startShenbuPool(t, env)
	if blocked, why := skip("ghost", acc); !blocked || !strings.Contains(why, "神捕") {
		t.Fatalf("重新启用：台账命中应恢复拦，实得 blocked=%v why=%q", blocked, why)
	}
	if n := ghostCandCount(t, env); n != 0 {
		t.Fatalf("重新启用：两号都应恢复让路，实得 %v", n)
	}
	if n := shenbuCandCount(t, env); n < 1 {
		t.Fatalf("台账号应仍在神捕候选里（重新启用后可续跑），实得 %v", n)
	}
}
