// 2026-09-29 #13（拍板 A）：背包预检"双证据并集" ——
//
//	① 权威信号：心跳 bag_full_age_ms ∈ (0, 30min]（最近被服务端以"包裹满"拒绝过）→ 避让；
//	② 兜底近似：bag 条目数 ≥50 → 避让（原闸保留，字段缺席/过期时仍拦囤积号）。
//
// 消费点：分享日常候选（/api/autotask candidates）+ roampool 回收资格（keeper Tick）。
// 契约：0/缺失 = 未知/从未 → 不拦（旧版机器人不误拦）；窗口外自愈（无需机器人端清零）。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// bagFullAgeEnv 造"两个 45 级、抓鬼满额"号的环境与差分取数器（只改 subject 的背包信号，
// twin 保持基线 → 计数变化归因唯一）。
func bagFullAgeEnv(t *testing.T) (env *testEnv, candShenbu func() float64, feed func(acc string, extra map[string]any)) {
	t.Helper()
	env = newTestEnv(t, "")
	addUsableAccount(t, env, "bag_subject@xy3.com", 45)
	addUsableAccount(t, env, "bag_twin@xy3.com", 45)
	feed = func(acc string, extra map[string]any) {
		t.Helper()
		st := map[string]any{"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
			"count_date": time.Now().Format("20060102")}}
		for k, v := range extra {
			st[k] = v
		}
		feedRobot(t, env, acc, st)
	}
	candShenbu = func() float64 {
		t.Helper()
		res := getJSON(t, env.srv.URL+"/api/autotask")
		cands, _ := res["candidates"].(map[string]any)
		n, _ := cands["shenbu"].(float64)
		return n
	}
	feed("bag_subject@xy3.com", nil)
	feed("bag_twin@xy3.com", nil)
	if n := candShenbu(); n != 2 {
		t.Fatalf("基线应 2 个候选，实际 %v", n)
	}
	return env, candShenbu, feed
}

func mkBagItems(n int) []any {
	bag := make([]any, n)
	for i := range bag {
		bag[i] = map[string]any{"id": float64(i + 1), "count": 1, "name": "杂物"}
	}
	return bag
}

// ① 窗口内命中：10 分钟前被"包满"拒过 → 候选剔除。
func TestBagFullAgeRecentBlocksCandidate(t *testing.T) {
	_, candShenbu, feed := bagFullAgeEnv(t)
	feed("bag_subject@xy3.com", map[string]any{"bag_full_age_ms": 10 * 60 * 1000})
	if n := candShenbu(); n != 1 {
		t.Fatalf("窗口内（10min）应避让（剩 1 个候选），实际 %v", n)
	}
}

// ① 窗口外自愈：31 分钟前被拒 → 放行（age 随时间增长自然过期，无需机器人端清零）。
func TestBagFullAgeExpiredSelfHeals(t *testing.T) {
	_, candShenbu, feed := bagFullAgeEnv(t)
	feed("bag_subject@xy3.com", map[string]any{"bag_full_age_ms": 31 * 60 * 1000})
	if n := candShenbu(); n != 2 {
		t.Fatalf("窗口外（31min）应自愈放行（回到 2 个候选），实际 %v", n)
	}
}

// ② 条目兜底：字段缺失 + bag 条目 ≥50 → 仍避让（原近似闸不撤）。
func TestBagCountGateStillBlocksWithoutAge(t *testing.T) {
	_, candShenbu, feed := bagFullAgeEnv(t)
	feed("bag_subject@xy3.com", map[string]any{"bag": mkBagItems(50)})
	if n := candShenbu(); n != 1 {
		t.Fatalf("无 age 字段时条目≥50 应兜底避让（剩 1 个候选），实际 %v", n)
	}
}

// 字段缺失/为 0：不误拦（旧版机器人不误伤；age=0 = 从未）；并验证"先报后撤"清残留
// （机器人重启/会话重建后不再带该字段 → 必须不误拦）。
func TestBagFullAgeMissingDoesNotBlock(t *testing.T) {
	_, candShenbu, feed := bagFullAgeEnv(t)
	// 缺席（基线已覆盖）→ 再显式给 0 与"过期大值"两个形态：
	feed("bag_subject@xy3.com", map[string]any{"bag_full_age_ms": 0})
	if n := candShenbu(); n != 2 {
		t.Fatalf("age=0（从未）不该拦，实际 %v", n)
	}
	feed("bag_subject@xy3.com", map[string]any{"bag_full_age_ms": 6 * 60 * 60 * 1000})
	if n := candShenbu(); n != 2 {
		t.Fatalf("age=6h（超窗）不该拦，实际 %v", n)
	}
	// 先命中窗口（剔除）→ 再撤回字段 → 残留必须清掉（否则 window 内一次命中会永久误拦）
	feed("bag_subject@xy3.com", map[string]any{"bag_full_age_ms": 10 * 60 * 1000})
	if n := candShenbu(); n != 1 {
		t.Fatalf("窗口内应避让（前置），实际 %v", n)
	}
	feed("bag_subject@xy3.com", nil)
	if n := candShenbu(); n != 2 {
		t.Fatalf("字段撤回后应清残留（不误拦），实际 %v", n)
	}
}

// 回收路径同判据：窗口内被拒的"抓鬼满额游荡号"不回收；过期后同一号可回收转投
// （正反两次 Tick 差分，证明是 age 信号在驱动）。
func TestBagFullAgeBlocksRoampoolDailyReclaim(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 600, TargetOnline: 3, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动大唐神捕池失败: %v", err)
	}
	acc := "dr_age@xy3.com"
	roamingGhostFull(t, env, acc, map[string]any{"bag_full_age_ms": 10 * 60 * 1000})
	k := newDailyReclaimKeeper(t, env, nil)

	if k.Tick(time.Now()) {
		t.Fatal("窗口内被'包满'拒过的游荡号不该被回收转投")
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("不该下发任何命令，实际 %v", cmd)
	}

	// 过期（31 分钟前）→ 自愈放行，同一号被回收转投
	roamingGhostFull(t, env, acc, map[string]any{"bag_full_age_ms": 31 * 60 * 1000})
	if !k.Tick(time.Now()) {
		t.Fatal("age 过期后应自愈放行、回收转投")
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" {
		t.Fatalf("应直发 share_daily_start: %v", cmd)
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != acc {
		t.Fatalf("应派回收选中的号 %s，实际 %v", acc, accs)
	}
}
