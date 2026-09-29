// 2026-09-29 P0-2 修法 B：游荡池"回收→日常池（神捕/烽火）"通道。
//
// 背景：神捕/烽火候选恒 0（号源断）——唯一放行的"抓鬼满额"号 94% 在游荡（walk.enabled）
// 被候选闸跳过；游荡池 running≈124 > target 100 有富余。修法 = keeper 把"日常池能接走"
// 的富余游荡号回收转投 share_daily_start（机器人端收到即停游荡，无抢断风险）。
//
// 判决顺序：**先抓鬼后日常** —— 抓鬼缺口命中（回收动作成功下发）时本轮不再动日常；
// 抓鬼缺口但挑不出号（满额/门槛外）时继续走日常；两个目标都挑不出号才合成一条 noop。
package roampool_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/state"
)

// 抓鬼不缺人、日常缺人 → 回收转投日常（停游荡由机器人端 share_daily_start 自己完成，
// keeper 只直发 ReclaimDaily，不走停游荡通道）。
func TestTickDailyReclaimWhenGhostHasNoDeficit(t *testing.T) {
	f := &fakeDeps{
		robots:       []state.Robot{walking("w1", 10), online("i1", 26)},
		deficit:      0,
		dailyDeficit: 2,
		maps:         []int{10, 26},
	}
	k := newKeeper(t, f, nil)
	if !k.Tick(time.Now()) {
		t.Fatal("日常池缺人时应下发回收转投")
	}
	if len(f.stopped) != 0 {
		t.Fatalf("抓鬼不缺人 → 不该走停游荡通道，实际 %v", f.stopped)
	}
	if len(f.dailyReclaimed) != 1 || join(f.dailyReclaimed[0]) != "w1" {
		t.Fatalf("应把 w1 回收转投日常，实际 %v", f.dailyReclaimed)
	}
	if len(f.disp) != 0 {
		t.Fatalf("回收轮不该同时补位，实际 %v", f.disp)
	}
	if !strings.Contains(k.Status().LastAction, "回收 1 个游荡号给日常池") {
		t.Fatalf("最近动作应说明回收转投日常，实际 %q", k.Status().LastAction)
	}
}

// 抓鬼缺口命中 → 先抓鬼：即使日常也有缺口，本轮也只走抓鬼回收（先抓鬼后日常）。
func TestTickGhostReclaimTakesPriorityOverDaily(t *testing.T) {
	f := &fakeDeps{
		robots:       []state.Robot{walking("w1", 10), walking("w2", 10)},
		deficit:      1,
		dailyDeficit: 5,
		maps:         []int{10},
	}
	k := newKeeper(t, f, nil)
	if !k.Tick(time.Now()) {
		t.Fatal("抓鬼缺口应触发回收")
	}
	if len(f.stopped) != 1 || join(f.stopped[0]) != "w1" {
		t.Fatalf("应先走抓鬼回收（停游荡），实际 %v", f.stopped)
	}
	if len(f.dailyReclaimed) != 0 {
		t.Fatalf("抓鬼通道命中时本轮不该再动日常，实际 %v", f.dailyReclaimed)
	}
	if !strings.Contains(k.Status().LastAction, "回收 1 个游荡号给任务池") {
		t.Fatalf("最近动作应是抓鬼回收，实际 %q", k.Status().LastAction)
	}
}

// 抓鬼缺口但挑不出可回收的号（资格闸全灭）→ 继续看日常池，不被抓鬼的 noop 截断。
func TestTickDailyReclaimRunsWhenGhostPicksNothing(t *testing.T) {
	f := &fakeDeps{
		robots:        []state.Robot{walking("w1", 10)},
		deficit:       3, // 抓鬼缺人，但资格闸下面会全灭
		dailyDeficit:  2,
		maps:          []int{10},
		roamEligible:  func(state.Robot) bool { return false }, // 抓鬼资格闸：全部不可回收
		dailyEligible: func(state.Robot) bool { return true },
	}
	k := newKeeper(t, f, nil)
	if !k.Tick(time.Now()) {
		t.Fatal("抓鬼无人可回收时，日常缺口应继续走回收转投")
	}
	if len(f.stopped) != 0 {
		t.Fatalf("抓鬼资格闸全灭 → 不该有停游荡动作，实际 %v", f.stopped)
	}
	if len(f.dailyReclaimed) != 1 || join(f.dailyReclaimed[0]) != "w1" {
		t.Fatalf("应由日常通道回收 w1，实际 %v", f.dailyReclaimed)
	}
}

// 日常回收资格闸 + 单轮上限：只挑闸放行的号，且 ≤ max_step。
func TestTickDailyReclaimRespectsEligibleAndMaxStep(t *testing.T) {
	f := &fakeDeps{
		robots:        []state.Robot{walking("w1", 10), walking("w2", 10), walking("w3", 26)},
		dailyDeficit:  10,
		maps:          []int{10, 26},
		dailyEligible: func(r state.Robot) bool { return r.Account != "w1" }, // w1 不合格
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.MaxStep = 1 })
	if !k.Tick(time.Now()) {
		t.Fatal("应下发日常回收")
	}
	if len(f.dailyReclaimed) != 1 || len(f.dailyReclaimed[0]) != 1 {
		t.Fatalf("单轮 ≤ max_step=1，实际 %v", f.dailyReclaimed)
	}
	if got := f.dailyReclaimed[0][0]; got == "w1" {
		t.Fatalf("资格闸不放行的 w1 不该被挑中，实际 %v", f.dailyReclaimed)
	}
}

// 两个目标都挑不出号 → 一条合并 noop（保留旧文案"没有可回收的游荡号"，面板可读）。
func TestTickReclaimNoopCoversBothPools(t *testing.T) {
	f := &fakeDeps{
		robots:       []state.Robot{online("i1", 10)},
		deficit:      2,
		dailyDeficit: 3,
		maps:         []int{10},
	}
	k := newKeeper(t, f, nil)
	if k.Tick(time.Now()) {
		t.Fatal("没有游荡号可回收时不该有动作")
	}
	act := k.Status().LastAction
	if !strings.Contains(act, "没有可回收的游荡号") {
		t.Fatalf("应说明为什么没动作，实际 %q", act)
	}
	if !strings.Contains(act, "日常池缺口 3") {
		t.Fatalf("noop 应同时报日常池缺口，实际 %q", act)
	}
}

// ReclaimDaily 下发失败（壳层报错）→ fail 日志 + 不误报成功。
func TestTickDailyReclaimFailureLogged(t *testing.T) {
	f := &fakeDeps{
		robots:       []state.Robot{walking("w1", 10)},
		dailyDeficit: 1,
		maps:         []int{10},
		dailyErr:     errors.New("通道未连接"),
	}
	k := newKeeper(t, f, nil)
	if k.Tick(time.Now()) {
		t.Fatal("日常回收下发失败时不该返回成功")
	}
	found := false
	for _, l := range f.logs {
		if strings.Contains(l, "回收→日常池失败") {
			found = true
		}
	}
	if !found {
		t.Fatalf("失败要落日志，实际 %v", f.logs)
	}
}

// 纯函数：PickDailyReclaim —— 资格闸过滤、n 上限、"领双不置顶"（双倍留给抓鬼通道）。
func TestPickDailyReclaimFilterAndNoDoublePriority(t *testing.T) {
	robots := []state.Robot{
		walking("a1", 10), // 图10 负载高（2 人）
		walking("a2", 10),
		walking("a3", 26),
	}
	// n=1：图负载高的图（10）上的号优先，并列按账号 → a1
	if got := roampool.PickDailyReclaim(robots, 1, nil, nil, 0); join(got) != "a1" {
		t.Fatalf("应挑图负载高的图上的号（账号升序 a1），实际 %v", got)
	}
	// a2 今日领双：抓鬼通道（PickReclaimAs）会把它置顶；日常通道不应受它影响
	robots2 := []state.Robot{
		walking("a1", 10),
		doubleToday(walking("a2", 10)),
		walking("a3", 26),
	}
	if got := roampool.PickDailyReclaim(robots2, 1, nil, nil, 0); join(got) != "a1" {
		t.Fatalf("领双号不该在日常通道置顶（仍应先挑 a1），实际 %v", got)
	}
	// 反证：抓鬼通道对同一批号会置顶领双号（口径差异是故意的）
	if got := roampool.PickReclaim(robots2, 1, nil); join(got) != "a2" {
		t.Fatalf("抓鬼通道仍应置顶领双号（a2），实际 %v", got)
	}
	// 资格闸：只放行 a3
	if got := roampool.PickDailyReclaim(robots, 5, func(r state.Robot) bool { return r.Account == "a3" }, nil, 0); join(got) != "a3" {
		t.Fatalf("资格闸应只放行 a3，实际 %v", got)
	}
	// n<=0 / 无人
	if got := roampool.PickDailyReclaim(robots, 0, nil, nil, 0); len(got) != 0 {
		t.Fatalf("n<=0 应返回空，实际 %v", got)
	}
}
