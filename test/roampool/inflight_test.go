// 2026-09-23 P1（派发节流降噪）：游荡派发的「在途记账 + 退避」与三类日志节流。
//
// 背景（实测）：机器人拒收定向派发（"给定选图全被排除"）时 keeper 完全不知情（通道发出即 sent），
// 每轮（现场 interval_sec=10）重挑同一批号 → 60 派/分、54 拒/分、同号同图 10s 一轮。
// 本组用例钉住：派发生效即清、在途占位不虚高、连续未生效按 60→120→300s 退避、离线即清、
// 且在途号不阻塞"回收/超编"通道。
package roampool_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/state"
)

// 派发生效（机器人上报 walk.enabled=true）→ 在途记录清除、退避解除。
func TestInflightClearedWhenWalkingStarts(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeDeps{robots: []state.Robot{online("i1", 10), online("i2", 10), online("i3", 10)}, maps: []int{10}}
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 2; c.MaxStep = 2; c.IntervalSec = 10 })

	if !k.Tick(base) {
		t.Fatal("余量号应被派去游荡")
	}
	if len(f.disp) != 1 || len(f.disp[0].accounts) != 2 {
		t.Fatalf("应派 2 个号（目标 2 / 单轮 2）：%+v", f.disp)
	}
	st := k.Status()
	if st.InflightPending != 2 || len(st.Inflight) != 2 {
		t.Fatalf("派发后应有 2 条在途占位：pending=%d inflight=%+v", st.InflightPending, st.Inflight)
	}
	if st.Inflight[0].Fails != 1 || !st.Inflight[0].Pending {
		t.Fatalf("首派应是第 1 档、占位中：%+v", st.Inflight[0])
	}

	// ① 在途占位：5s 后再跑 —— 占位 2 = 目标 2 → 达标，不再叠派一批（旧行为会再派）
	f.now = base.Add(5 * time.Second)
	if k.Tick(f.now) {
		t.Fatal("在途占位已满目标时不该再有动作")
	}
	if len(f.disp) != 1 {
		t.Fatalf("在途号不该被重复派：%+v", f.disp)
	}
	if got := k.Status().LastAction; !strings.Contains(got, "达标") {
		t.Fatalf("动作文案应说明达标：%q", got)
	}

	// ② 机器人上报 walk.enabled=true（生效）→ 记录清除（退避档位随之归零）
	for i := range f.robots {
		f.robots[i].Walk = map[string]any{"enabled": true}
	}
	f.now = base.Add(10 * time.Second)
	k.Tick(f.now)
	st = k.Status()
	if st.InflightPending != 0 || len(st.Inflight) != 0 {
		t.Fatalf("派发生效后在途应清空：pending=%d inflight=%+v", st.InflightPending, st.Inflight)
	}
	if st.Running != 3 {
		t.Fatalf("三个号都应在游荡：%+v", st)
	}
}

// 连续"派发未生效"按 60 → 120 → 300s 退避（末档为上限）；期间不重复派。
func TestInflightBackoffEscalation(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeDeps{robots: []state.Robot{online("i1", 10)}, maps: []int{10}}
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 1; c.MaxStep = 1; c.IntervalSec = 10 })

	cases := []struct {
		at   int // 相对 base 的秒数
		sent bool
	}{
		{0, true},    // 空账 → 第 1 次派发（窗口 60s）
		{30, false},  // 退避窗口内
		{61, true},   // 第 2 次（上一档没生效 → 升到 120s）
		{120, false}, // 120s 窗口内
		{182, true},  // 第 3 次（升到 300s = 上限）
		{300, false}, // 300s 窗口内
		{400, false}, // 仍在窗口内
		{484, true},  // 第 4 次（档位封顶，仍是 300s）
	}
	dispatched := 0
	for _, c := range cases {
		f.now = base.Add(time.Duration(c.at) * time.Second)
		before := len(f.disp)
		if acted := k.Tick(f.now); acted != c.sent {
			t.Fatalf("%ds：期望下发=%v，实际=%v（已下发 %+v；logs=%v）", c.at, c.sent, acted, f.disp, f.logs)
		}
		if len(f.disp) > before {
			dispatched++
		}
	}
	if dispatched != 4 {
		t.Fatalf("窗口外各派一次，共 4 次；实际 %d（%+v）", dispatched, f.disp)
	}
	st := k.Status()
	if len(st.Inflight) != 1 || st.Inflight[0].Fails != 3 {
		t.Fatalf("退避档位应封顶在第 3 档：%+v", st.Inflight)
	}
	if d := st.Inflight[0].Until.Sub(base.Add(484 * time.Second)); d != 300*time.Second {
		t.Fatalf("封顶后窗口应为 300s，实际 %v", d)
	}
}

// 离线即清（这号不在池子里了）：回来后再派从第 1 档重新起算。
func TestInflightClearedOnOffline(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeDeps{robots: []state.Robot{online("i1", 10)}, maps: []int{10}}
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 1; c.MaxStep = 1; c.IntervalSec = 10 })

	k.Tick(base)
	if st := k.Status(); len(st.Inflight) != 1 {
		t.Fatalf("应先记一条在途：%+v", st.Inflight)
	}
	// 机器人掉线（心跳过期 → Online=false）
	f.robots[0].Online = false
	f.now = base.Add(20 * time.Second)
	k.Tick(f.now)
	if st := k.Status(); len(st.Inflight) != 0 {
		t.Fatalf("离线后应清掉在途记录：%+v", st.Inflight)
	}
	// 重新上线（空闲）→ 可再派，且是第 1 档
	f.robots[0].Online = true
	f.now = base.Add(30 * time.Second)
	if !k.Tick(f.now) {
		t.Fatal("重新上线后应能再派")
	}
	if st := k.Status(); len(st.Inflight) != 1 || st.Inflight[0].Fails != 1 {
		t.Fatalf("离线清除后应从第 1 档重新起算：%+v", st.Inflight)
	}
}

// 在途号不阻塞两条既有通道：① 回收（游荡号还给任务池）；② 超编收敛（抓鬼号转游荡）。
func TestInflightDoesNotBlockReclaimOrExcess(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	// ① 回收优先：在途记录存在时，缺口>0 仍应立即回收在游荡的号
	f := &fakeDeps{
		robots:  []state.Robot{walking("w1", 10), online("i1", 10)},
		deficit: 0, maps: []int{10}, // 先不缺口 → 能补位；随后把缺口打开验证回收
	}
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 3; c.MaxStep = 3; c.IntervalSec = 10 })
	k.Tick(base) // 先派一个（i1 进在途）
	if len(f.disp) != 1 || len(f.disp[0].accounts) != 1 || f.disp[0].accounts[0] != "i1" {
		t.Fatalf("首轮应只派空闲号 i1：%+v", f.disp)
	}
	f.deficit = 2 // 任务池缺人 → 回收优先
	f.now = base.Add(5 * time.Second)
	if !k.Tick(f.now) {
		t.Fatal("缺口>0 时应回收游荡号")
	}
	if len(f.stopped) != 1 || join(f.stopped[0]) != "w1" {
		t.Fatalf("在途记录不该挡住回收：%+v", f.stopped)
	}

	// ② 超编收敛：在途的空闲号不再重复派，超编号（可中断）照常转游荡
	f2 := &fakeDeps{
		robots:  []state.Robot{online("i1", 10), ghostAt("g1", "WAIT_GHOST")},
		deficit: -5, maps: []int{10},
	}
	f2.now = base
	k2 := newKeeper(t, f2, func(c *roampool.Config) { c.Target = 5; c.MaxStep = 1; c.IntervalSec = 10 })
	k2.Tick(base) // 首轮派 i1（占位）
	if len(f2.disp) != 1 || f2.disp[0].accounts[0] != "i1" {
		t.Fatalf("首轮应派 i1：%+v", f2.disp)
	}
	f2.now = base.Add(5 * time.Second)
	if !k2.Tick(f2.now) {
		t.Fatal("第二轮应挑超编号 g1（i1 在途被排除）")
	}
	if len(f2.disp) != 2 || f2.disp[1].accounts[0] != "g1" {
		t.Fatalf("在途号不该重复派、超编号该照常转游荡：%+v", f2.disp)
	}
}

// ---------------------------------------------------------------- 日志降噪（P1 同批）

// 达标行：状态翻转时打一条 + 心跳（600s）；稳定期连续轮次不再重复打，但 lastAct 每轮都更新。
func TestLogGoalThrottled(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeDeps{robots: []state.Robot{walking("w1", 10), walking("w2", 10)}, maps: []int{10}}
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 2; c.IntervalSec = 10 })

	for i := 0; i < 12; i++ {
		f.now = base.Add(time.Duration(i*10) * time.Second)
		k.Tick(f.now)
		if got := k.Status().LastAction; !strings.Contains(got, "达标") {
			t.Fatalf("第 %d 轮 lastAct 应仍更新为达标文案：%q", i, got)
		}
	}
	if n := countLogs(f.logs, "达标"); n != 1 {
		t.Fatalf("120s 内达标行应只打 1 条（翻转），实际 %d：%v", n, f.logs)
	}
	// 翻转回"未达标"再达标 → 立刻再打一条
	f.robots = []state.Robot{online("i1", 10)}
	f.now = base.Add(130 * time.Second)
	k.Tick(f.now) // 无号可派 → no-op（goalState 复位）
	f.robots = []state.Robot{walking("w1", 10), walking("w2", 10)}
	f.now = base.Add(140 * time.Second)
	k.Tick(f.now)
	if n := countLogs(f.logs, "达标"); n != 2 {
		t.Fatalf("状态翻转后应立刻再打一条：%d：%v", n, f.logs)
	}
}

// 空转行：同「类型+缺口档+空闲档」300s 内只打一条；档位变了要打新的。
func TestLogNoopThrottledByBucket(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeDeps{robots: []state.Robot{online("i1", 10), online("i2", 10)}, deficit: 2, maps: []int{10}}
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 5; c.IntervalSec = 10 })

	for i := 0; i < 10; i++ { // 缺口 2、空闲 2 → 同一档位键
		f.now = base.Add(time.Duration(i*10) * time.Second)
		k.Tick(f.now)
	}
	if n := countLogs(f.logs, "没有可回收的游荡号"); n != 1 {
		t.Fatalf("同档位空转行 300s 内只该 1 条，实际 %d：%v", n, f.logs)
	}
	// 空闲数跨档（2 → 6，notch 从 "1-4" 变 "5-9"）→ 新键，应再打一条
	robots := []state.Robot{}
	for i := 0; i < 6; i++ {
		robots = append(robots, online(string(rune('a'+i))+"@x", 10))
	}
	f.robots = robots
	f.now = base.Add(100 * time.Second)
	k.Tick(f.now)
	if n := countLogs(f.logs, "没有可回收的游荡号"); n != 2 {
		t.Fatalf("空闲档位变化应打新的一条：%d：%v", n, f.logs)
	}
}

// 动作行：60s 窗口内聚合 —— 首条原样，之后被抑制并在下一条带上"近60s 另：…"前缀。
func TestLogActionsAggregated(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeDeps{maps: []int{10}} // 每轮补一个空闲号
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 99; c.MaxStep = 1; c.IntervalSec = 10 })

	for i := 0; i < 5; i++ {
		f.now = base.Add(time.Duration(i*10) * time.Second)
		f.robots = append(f.robots, online("i"+string(rune('0'+i)), 10))
		k.Tick(f.now)
	}
	if n := countLogs(f.logs, "补位"); n != 1 {
		t.Fatalf("60s 内动作行应只打 1 条，实际 %d：%v", n, f.logs)
	}
	// 窗口到期后的第一条：带"近60s 另：补位 4 次/4 个"前缀（前 4 轮被抑制）
	f.robots = append(f.robots, online("i9", 10))
	f.now = base.Add(61 * time.Second)
	k.Tick(f.now)
	if n := countLogs(f.logs, "补位"); n != 2 {
		t.Fatalf("窗口到期应打第 2 条，实际 %d：%v", n, f.logs)
	}
	last := f.logs[len(f.logs)-1]
	if !strings.Contains(last, "近60s 另：补位 4 次/4 个") {
		t.Fatalf("第 2 条应带被抑制量的前缀：%q", last)
	}
	if got := k.Status().LastAction; !strings.Contains(got, "补位 1 个") {
		t.Fatalf("lastAct 应是最后一条动作文案：%q", got)
	}
}

// ---------------------------------------------------------------- 空转退避（B1）

// 空转退避：无活可干才累积（Start 会把间隔 ×2 到 60s 上限）；一有活可干（可回收/可补位）立刻归零
// —— 保证任务池缺人时仍是 interval_sec 的节奏（不延迟回收，尤其领双号）。
func TestIdleStreakBacksOffOnlyWhenNothingActionable(t *testing.T) {
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeDeps{robots: []state.Robot{walking("w1", 10)}, maps: []int{10}}
	f.now = base
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 1; c.IntervalSec = 10 })

	for i := 0; i < 4; i++ { // 达标（在游荡 1 = 目标 1）→ 空转
		f.now = base.Add(time.Duration(i*10) * time.Second)
		k.Tick(f.now)
	}
	if got := k.Status().IdleStreak; got != 4 {
		t.Fatalf("达标空转应累积到 4，实际 %d", got)
	}
	// 有活可干：缺口>0 且有游荡号可回收 → 立刻回收且空转归零（节奏回到 interval_sec）
	f.deficit = 3
	f.now = base.Add(50 * time.Second)
	if !k.Tick(f.now) {
		t.Fatal("缺口>0 且有游荡号时应回收")
	}
	if got := k.Status().IdleStreak; got != 0 {
		t.Fatalf("有动作后空转应归零，实际 %d", got)
	}
	// 无候选可回收（缺口>0 但没有游荡号）→ 仍是空转（Start 会拉长间隔），但下轮一旦有号立刻恢复
	f.robots = []state.Robot{online("i1", 10)}
	f.now = base.Add(60 * time.Second)
	k.Tick(f.now)
	if got := k.Status().IdleStreak; got != 1 {
		t.Fatalf("缺口但没有可回收号 → 空转累积 1，实际 %d", got)
	}
}

// ---------------------------------------------------------------- 小工具

// countLogs 日志里含某子串的行数（节流断言用）。
func countLogs(logs []string, sub string) int {
	n := 0
	for _, l := range logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}
