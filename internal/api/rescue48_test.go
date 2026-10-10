// 47/48 图自动救援 tick + 错峰抓鬼（2026-10-10）纯逻辑单测：
// 资格过滤 / 频控（30min 节流、日 ≤3、跨日清零）/ 排序截断与排队 /
// 错峰 offset 分布与 ready 边界 / 灰度取样确定性 / 到点拉回仅作用于纯游荡。
package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/state"
)

func TestRescue48EligibleFilter(t *testing.T) {
	base := state.Robot{Account: "r1@xy3.com", Online: true, MapID: 47}
	cases := []struct {
		name string
		mut  func(*state.Robot)
		want bool
	}{
		{"命中 47", func(r *state.Robot) {}, true},
		{"命中 48", func(r *state.Robot) { r.MapID = 48 }, true},
		{"离线", func(r *state.Robot) { r.Online = false }, false},
		{"图 45 不命中", func(r *state.Robot) { r.MapID = 45 }, false},
		{"图 46 不命中", func(r *state.Robot) { r.MapID = 46 }, false},
		{"图 0 不命中", func(r *state.Robot) { r.MapID = 0 }, false},
		{"人工暂停", func(r *state.Robot) { r.Paused = true }, false},
		{"当日卡死熔断", func(r *state.Robot) { r.RestoreCapped = true }, false},
	}
	for _, c := range cases {
		r := base
		c.mut(&r)
		if got := rescue48Eligible(r); got != c.want {
			t.Fatalf("%s：rescue48Eligible=%v 期望 %v（%+v）", c.name, got, c.want, r)
		}
	}
}

func TestRescue48LimiterThrottle(t *testing.T) {
	lim := newRescue48Limiter()
	acc := "r2@xy3.com"
	t0 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local)

	if ok, why := lim.allow(acc, t0); !ok {
		t.Fatalf("首次应放行：%s", why)
	}
	lim.mark(acc, t0)

	// 29 分钟：30min 节流拒绝（带原因，供留痕）
	if ok, why := lim.allow(acc, t0.Add(29*time.Minute)); ok || why == "" {
		t.Fatalf("29 分钟应被节流拒绝并带原因，实得 ok=%v why=%q", ok, why)
	}
	// 30 分钟整：放行（边界 >=）
	if ok, _ := lim.allow(acc, t0.Add(30*time.Minute)); !ok {
		t.Fatalf("30 分钟整应放行（边界）")
	}
	lim.mark(acc, t0.Add(30*time.Minute))
	lim.mark(acc, t0.Add(60*time.Minute))
	// 今日已 3 发 → 第 4 发被日上限拒绝
	if ok, why := lim.allow(acc, t0.Add(90*time.Minute)); ok || !strings.Contains(why, "上限") {
		t.Fatalf("第 4 发应被日上限拒绝并带原因，实得 ok=%v why=%q", ok, why)
	}
	// 跨日清零 → 放行
	nextDay := time.Date(2026, 10, 11, 0, 1, 0, 0, time.Local)
	if ok, why := lim.allow(acc, nextDay); !ok {
		t.Fatalf("跨日应清零放行：%s", why)
	}
	// 其它号不受影响
	if ok, why := lim.allow("other@xy3.com", t0); !ok {
		t.Fatalf("其它号不应受影响：%s", why)
	}
}

func TestRescue48ScanFilterThrottleCut(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	lim := newRescue48Limiter()
	lim.mark("t1@xy3.com", now.Add(-5*time.Minute)) // 距离上次仅 5 分钟 → 节流
	// 输入乱序，验证输出升序；t2=已移除、t4=暂停、t5=图45、t6=离线 均不该入选
	snap := []state.Robot{
		{Account: "t7@xy3.com", Online: true, MapID: 48},
		{Account: "t3@xy3.com", Online: true, MapID: 47},
		{Account: "t1@xy3.com", Online: true, MapID: 47},
		{Account: "t2@xy3.com", Online: true, MapID: 47},
		{Account: "t4@xy3.com", Online: true, MapID: 47, Paused: true},
		{Account: "t5@xy3.com", Online: true, MapID: 45},
		{Account: "t6@xy3.com", Online: false, MapID: 47},
	}
	removed := func(acc string) bool { return acc == "t2@xy3.com" }

	batch, throttled, reasons, queued := rescue48Scan(now, snap, lim, removed, 1)
	if len(batch) != 1 || batch[0] != "t3@xy3.com" {
		t.Fatalf("max=1 应只下发升序第一个 t3，实得 %v", batch)
	}
	if queued != 1 {
		t.Fatalf("已放行超额应记排队 1（t7），实得 %d", queued)
	}
	if len(throttled) != 1 || throttled[0] != "t1@xy3.com" || !strings.Contains(reasons[0], "距上次下发") {
		t.Fatalf("t1 应被频控跳过并带原因，实得 %v / %v", throttled, reasons)
	}
	for _, acc := range append(append([]string{}, batch...), throttled...) {
		if acc == "t2@xy3.com" || acc == "t4@xy3.com" || acc == "t5@xy3.com" || acc == "t6@xy3.com" {
			t.Fatalf("不该入选的号 %s 出现在名单里（batch=%v throttled=%v）", acc, batch, throttled)
		}
	}

	// 不截断时：t3/t7 全部下发，无排队
	batch2, _, _, queued2 := rescue48Scan(now, snap, lim, removed, 10)
	if len(batch2) != 2 || batch2[0] != "t3@xy3.com" || batch2[1] != "t7@xy3.com" || queued2 != 0 {
		t.Fatalf("max=10 应下发 [t3 t7] 无排队，实得 %v queued=%d", batch2, queued2)
	}
}

func TestGhostStaggerOffsetDistribution(t *testing.T) {
	seen := map[int64]bool{}
	minOff, maxOff := int64(1<<62), int64(-1)
	for i := 0; i < 2000; i++ {
		off := ghostStaggerOffsetSec(fmt.Sprintf("robot%07d@xy3.com", i))
		if off < 0 || off >= ghostStaggerDaySec {
			t.Fatalf("offset 越界：%d", off)
		}
		seen[off] = true
		if off < minOff {
			minOff = off
		}
		if off > maxOff {
			maxOff = off
		}
	}
	if len(seen) < 1000 {
		t.Fatalf("分布过窄：2000 个号只落 %d 个不同 offset", len(seen))
	}
	if minOff > 3600 || maxOff < 16*3600 {
		t.Fatalf("覆盖范围过窄：min=%d max=%d（应铺满 0~18h）", minOff, maxOff)
	}
	// 确定性：同号结果稳定
	if ghostStaggerOffsetSec("robot0000001@xy3.com") != ghostStaggerOffsetSec("robot0000001@xy3.com") {
		t.Fatalf("同号 offset 应确定")
	}
}

func TestGhostStaggerReadyBoundaries(t *testing.T) {
	acc := "robot0000042@xy3.com"
	off := ghostStaggerOffsetSec(acc)
	day0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.Local)

	// 0 点整：仅 offset=0 的号 ready
	if off == 0 && !ghostStaggerReady(acc, day0) {
		t.Fatalf("offset=0 的号 0 点应 ready")
	}
	if off > 0 && ghostStaggerReady(acc, day0) {
		t.Fatalf("offset=%d 的号 0 点不该 ready", off)
	}
	// 到点瞬间（0 点 + offset unix）：ready（边界 >=）；前 1 秒：不 ready
	at := time.Unix(day0.Unix()+off, 0)
	if !ghostStaggerReady(acc, at) {
		t.Fatalf("到点瞬间应 ready")
	}
	if off > 0 && ghostStaggerReady(acc, time.Unix(day0.Unix()+off-1, 0)) {
		t.Fatalf("到点前 1 秒不该 ready")
	}
	// 23:59:59：全量 ready（最大 offset 17:59:59 已过）
	if !ghostStaggerReady(acc, time.Date(2026, 10, 10, 23, 59, 59, 0, time.Local)) {
		t.Fatalf("23:59:59 应全量 ready")
	}
	// 跨日：次日 0 点回到未 ready（offset>0 时）
	next0 := time.Date(2026, 10, 11, 0, 0, 0, 0, time.Local)
	if off > 0 && ghostStaggerReady(acc, next0) {
		t.Fatalf("次日 0 点不该 ready（新一轮错峰）")
	}
}

func TestGhostStaggerCoveredDeterministic(t *testing.T) {
	acc := "robot0000500@xy3.com"
	raw := int(fnv1a32("s"+acc) % 100)
	for pct := 0; pct <= 100; pct++ {
		want := pct > 0 && (pct >= 100 || raw < pct)
		if got := ghostStaggerCovered(acc, pct); got != want {
			t.Fatalf("pct=%d 取样不一致：got=%v want=%v", pct, got, want)
		}
	}
	if ghostStaggerCovered(acc, 0) {
		t.Fatalf("pct=0 不该有号受约束")
	}
	if !ghostStaggerCovered(acc, 100) {
		t.Fatalf("pct=100 应全量受约束")
	}
	// 单调扩张：50 命中的号在 80 必然命中
	if ghostStaggerCovered(acc, 50) && !ghostStaggerCovered(acc, 80) {
		t.Fatalf("灰度应随 pct 单调扩张")
	}
	// 分布合理：1000 个号在 pct=50 命中约 50%（容差 35%~65%）
	n := 0
	for i := 0; i < 1000; i++ {
		if ghostStaggerCovered(fmt.Sprintf("robot%07d@xy3.com", i), 50) {
			n++
		}
	}
	if n < 350 || n > 650 {
		t.Fatalf("灰度分布异常：1000 个号 pct=50 命中 %d", n)
	}
}

func TestGhostStaggerPullBackOnlyPureRoam(t *testing.T) {
	roaming := state.Robot{Account: "p1@xy3.com", Online: true, Walk: map[string]any{"enabled": true}}
	if !ghostStaggerPullBack(true, true, roaming) {
		t.Fatalf("受约束+到点+纯游荡 应豁免 Busy")
	}
	if ghostStaggerPullBack(false, true, roaming) {
		t.Fatalf("未受约束不豁免")
	}
	if ghostStaggerPullBack(true, false, roaming) {
		t.Fatalf("未到点不豁免")
	}
	fighting := roaming
	fighting.Fight = true
	if ghostStaggerPullBack(true, true, fighting) {
		t.Fatalf("战斗中不豁免")
	}
	hatch := state.Robot{Account: "p2@xy3.com", Online: true, Hatch: map[string]any{"active": true}}
	if ghostStaggerPullBack(true, true, hatch) {
		t.Fatalf("孵化中不豁免（Walking 含孵化，按纯游荡语义排除）")
	}
	ghostRun := roaming
	ghostRun.Ghost = map[string]any{"enabled": true}
	if ghostStaggerPullBack(true, true, ghostRun) {
		t.Fatalf("活跃抓鬼会话不豁免")
	}
	if ghostStaggerPullBack(true, true, state.Robot{Account: "p3@xy3.com", Online: true}) {
		t.Fatalf("非游荡不豁免")
	}
	if ghostStaggerPullBack(true, true,
		state.Robot{Account: "p4@xy3.com", Online: true, Walk: map[string]any{"enabled": false}}) {
		t.Fatalf("walk.enabled=false 不豁免")
	}
}
