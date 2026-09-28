// 2026-09-28 穷号闸（G1/G2/G3）接口层用例 —— 储备金 < 阈值的号不派/不补/不回收抓鬼：
//
//	背景（docs/04-测试/分析-20260928-商店买药卡住排查.md）：23 个穷号(储备金 56~34277)
//	在药铺"552(货币不够)→90 秒超时→60 秒冷却→再买"死循环里空转(5xxx 号买 200 个金创药,
//	单价 211 买不起)；穷号被派/补/回收去抓鬼 = 继续空转，无产出。
//
// 本组用例钉住三处（判据 = Reserve>0 且 < 阈值(GhostReserveFloor，默认 500)）：
//
//	① G1 抓鬼候选（autotaskCandidatesCfg）：穷号不进 ghosts 候选；reserve=0（未同步）/
//	   reserve≥阈值（含等于）→ 照常进；补钱后下一轮回到候选（无粘滞）。
//	② G2 恢复引擎闸（GhostSkipFunc）：穷号 skip + 原因；同上反例；非 ghost kind 不受影响。
//	③ G3 游荡池回收资格（RoampoolDeps().ReclaimEligible）：穷号不回收（留在游荡），
//	   reserve=0/富号照常可回收。
package api_test

import (
	"strings"
	"testing"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
)

// ① G1：抓鬼候选按储备金过滤。
func TestGhostReserveFloorCandidate(t *testing.T) {
	env := newTestEnv(t, "")
	rich, poor, unsynced, boundary := "gres_rich@xy3.com", "gres_poor@xy3.com",
		"gres_zero@xy3.com", "gres_boundary@xy3.com"
	env.pool.Add([]string{rich, poor, unsynced, boundary}, "", testZoneAddr, "")
	for _, a := range []string{rich, poor, unsynced, boundary} { // 都毕业(45)+可用 → 基线都能进
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45})
	}
	feedRobot(t, env, rich, map[string]any{"reserve": 1000})    // 富 → 进候选
	feedRobot(t, env, poor, map[string]any{"reserve": 100})     // 穷(<500) → 不进
	feedRobot(t, env, unsynced, nil)                            // reserve 缺席(=0) → 不拦
	feedRobot(t, env, boundary, map[string]any{"reserve": 500}) // = 阈值 → 不拦(判据是 <)

	if n := ghostCandCount(t, env); n != 3 {
		t.Fatalf("穷号(reserve=100)应被过滤，其余 3 个(含未同步/等于阈值)应在候选，实得 %v", n)
	}
	// 人工补钱/接任务赚钱后 → 下一轮自动回到候选（无粘滞）
	feedRobot(t, env, poor, map[string]any{"reserve": 1000})
	if n := ghostCandCount(t, env); n != 4 {
		t.Fatalf("补钱后穷号应自动回到抓鬼候选（无粘滞），实得 %v", n)
	}
}

// ② G2：恢复引擎闸（restorer 补发前最后一道闸）按储备金拦 ghost。
func TestGhostSkipFuncReserve(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	poor, rich, zero := "gres_skip_poor@xy3.com", "gres_skip_rich@xy3.com", "gres_skip_zero@xy3.com"
	feedRobot(t, env, poor, map[string]any{"reserve": 120})
	feedRobot(t, env, rich, map[string]any{"reserve": 900})
	feedRobot(t, env, zero, nil)

	skip := env.api.GhostSkipFunc()
	if blocked, why := skip("ghost", poor); !blocked || !strings.Contains(why, "储备金不足") {
		t.Fatalf("穷号应被拦下补发并说明原因，实得 blocked=%v why=%q", blocked, why)
	}
	if blocked, why := skip("ghost", rich); blocked {
		t.Fatalf("富号不该被拦补发，实被拦: %s", why)
	}
	if blocked, why := skip("ghost", zero); blocked {
		t.Fatalf("reserve 缺席(=0，未同步)不该被拦补发，实被拦: %s", why)
	}
	// 号有钱后自动放行（人工补钱即可回归抓鬼）
	feedRobot(t, env, poor, map[string]any{"reserve": 5000})
	if blocked, why := skip("ghost", poor); blocked {
		t.Fatalf("补钱后不该再拦补发，实被拦: %s", why)
	}
	// 只对 ghost 生效：非抓鬼 kind 不受穷号闸影响（本函数对其它 kind 直通）
	if blocked, why := skip("newbie", poor); blocked {
		t.Fatalf("非 ghost kind 不该被穷号闸拦，实被拦: %s", why)
	}
}

// ③ G3：游荡池"任务缺人回收"资格按储备金拦。
func TestRoamReclaimEligibleReserve(t *testing.T) {
	env := newTestEnv(t, "")
	deps := env.api.RoampoolDeps()
	if deps.ReclaimEligible == nil {
		t.Fatal("RoampoolDeps().ReclaimEligible 未装配")
	}
	poor := state.Robot{Account: "gres_rp_poor@xy3.com", Reserve: 150}
	rich := state.Robot{Account: "gres_rp_rich@xy3.com", Reserve: 900}
	zero := state.Robot{Account: "gres_rp_zero@xy3.com"}

	if deps.ReclaimEligible(poor) {
		t.Fatal("穷号不该被回收（回收去抓鬼只会继续买药空转）")
	}
	if !deps.ReclaimEligible(rich) {
		t.Fatal("富号应可回收（正常补任务池）")
	}
	if !deps.ReclaimEligible(zero) {
		t.Fatal("reserve 缺席(=0)不该拦（保守放行，机器人端兜底）")
	}
}
