// roampool 用例：纯函数（Roaming / Idle / PickReclaim / PickIdle / BalanceAssign / MapLoads）
// + 一轮决策 Tick（回收优先 / 按图均匀补位 / enabled=false 什么都不做 / 单图失败不影响其它图）
// + 参数落盘与环境变量覆盖。
//
// 全部用假依赖（不联网、不起进程、不碰真实 data/）：断言的是**结果证据**——
// 挑中了哪些账号、分配到哪张图、下发了什么、日志说了什么、落盘文件里读回什么。
package roampool_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 造数

func online(acc string, mapid int) state.Robot {
	return state.Robot{Account: acc, Online: true, MapID: mapid, State: "ONLINE", HS: true}
}

// walking 游荡中的号（机器人上报 walk.enabled=true）。
func walking(acc string, mapid int) state.Robot {
	r := online(acc, mapid)
	r.Walk = map[string]any{"enabled": true, "mapid": mapid, "state": "walk"}
	return r
}

// hatching 孵化中的号：机器人端内部也是游荡（walk.enabled=true），但**不算游荡池**（不该回收它）。
func hatching(acc string, mapid int) state.Robot {
	r := walking(acc, mapid)
	r.Hatch = map[string]any{"active": true, "kind": "egg", "mapid": mapid}
	return r
}

func ghosting(acc string) state.Robot {
	r := online(acc, 10)
	r.Ghost = map[string]any{"enabled": true}
	return r
}

func fighting(acc string) state.Robot {
	r := online(acc, 10)
	r.Fight = true
	return r
}

func join(accs []string) string { return strings.Join(accs, ",") }

// ---------------------------------------------------------------- 纯函数：在游荡 / 空闲

func TestRoamingAndIdleJudgements(t *testing.T) {
	if !roampool.Roaming(walking("a", 10)) {
		t.Fatal("walk.enabled=true 应算在游荡")
	}
	// 2026-09-22 用户口径：**孵化号也可以回收做任务**（没蛋孵不了；停了不损失进度）→
	//   孵化中的号**算**游荡池（可被回收）。
	if !roampool.Roaming(hatching("a", 6)) {
		t.Fatal("孵化中的号应算游荡池（用户口径：孵化的也可以回收做任务）")
	}
	if roampool.Roaming(online("a", 10)) {
		t.Fatal("普通在线号不算在游荡")
	}
	off := walking("a", 10)
	off.Online = false
	if roampool.Roaming(off) {
		t.Fatal("离线号不算在游荡（心跳过期/已下机）")
	}
	if !roampool.Idle(online("a", 10)) {
		t.Fatal("在线且不忙的号应算空闲")
	}
	for _, r := range []state.Robot{ghosting("a"), fighting("a"), walking("a", 10), hatching("a", 6)} {
		if roampool.Idle(r) {
			t.Fatalf("抓鬼/战斗/游荡/孵化的号不该算空闲: %+v", r)
		}
	}
	// 2026-09-23（前端 26f4660 同口径）：交付中（SUBMIT）与卡住（ERROR）也不算空闲 ——
	// 前者在推进、后者是异常（先人工处理）；判据经 waterline.Busy 统一。
	submit := online("s1", 10)
	submit.State = "SUBMIT"
	if roampool.Idle(submit) {
		t.Fatal("交付中（SUBMIT）不该算空闲：派游荡会打断交付")
	}
	stuck := online("e1", 10)
	stuck.State = "ERROR"
	if roampool.Idle(stuck) {
		t.Fatal("卡住（ERROR）不该算空闲：异常号先人工处理，别派游荡")
	}
	// 对照：等刷鬼的等待段（WAIT_GHOST，无活跃会话）仍算空闲（与 Interruptible 白名单一致）
	waitGhost := online("w1", 10)
	waitGhost.State = "WAIT_GHOST"
	if !roampool.Idle(waitGhost) {
		t.Fatal("WAIT_GHOST 且无活跃抓鬼会话 = 等待段，应算空闲")
	}
	task := online("a", 10)
	task.State = "NAV"
	if roampool.Idle(task) {
		t.Fatal("任务态（NAV）不该算空闲")
	}
	waitTask := online("a", 10)
	waitTask.State, waitTask.TaskIndex = "WAIT_TASK", 3
	if roampool.Idle(waitTask) {
		t.Fatal("WAIT_TASK 且任务索引非 0（任务进行中）不该算空闲")
	}
}

// ---------------------------------------------------------------- 纯函数：PickReclaim

func TestPickReclaimPrefersCrowdedMap(t *testing.T) {
	robots := []state.Robot{
		walking("a1", 10), walking("a2", 10), online("a3", 10), // 图10：3 个号（其中 2 个在游荡）
		walking("b1", 26), // 图26：1 个
		walking("c1", 24), // 图24：1 个
	}
	got := roampool.PickReclaim(robots, 2, nil)
	if join(got) != "a1,a2" {
		t.Fatalf("应先回收人最多的图上的游荡号（图10 的 a1/a2），实际 %v", got)
	}
}

func TestPickReclaimShortfallAndEmpty(t *testing.T) {
	robots := []state.Robot{walking("b1", 26), online("x", 10)}
	got := roampool.PickReclaim(robots, 5, nil)
	if len(got) != 1 || got[0] != "b1" {
		t.Fatalf("游荡号不够时有多少给多少，实际 %v", got)
	}
	if got := roampool.PickReclaim([]state.Robot{online("x", 10), ghosting("y")}, 3, nil); len(got) != 0 {
		t.Fatalf("没有游荡号应返回空，实际 %v", got)
	}
	if got := roampool.PickReclaim(robots, 0, nil); len(got) != 0 {
		t.Fatalf("n<=0 应返回空，实际 %v", got)
	}
}

func TestPickReclaimTieBreakIsStable(t *testing.T) {
	// 两张图人数相同 → 按账号升序，结果稳定（日志/测试可预期）
	robots := []state.Robot{walking("z9", 10), walking("a1", 24)}
	if got := roampool.PickReclaim(robots, 1, nil); join(got) != "a1" {
		t.Fatalf("并列时应按账号升序取第一个，实际 %v", got)
	}
}

// ---------------------------------------------------------------- 纯函数：PickIdle

func TestPickIdleFiltersBusyAndOffline(t *testing.T) {
	waitTask := online("t2", 10)
	waitTask.State, waitTask.TaskIndex = "WAIT_TASK", 5
	off := online("off1", 10)
	off.Online = false
	submit := online("s1", 10)
	submit.State = "SUBMIT" // 2026-09-23：交付中 = 推进中，不派游荡
	stuck := online("e1", 10)
	stuck.State = "ERROR" // 2026-09-23：卡住 = 异常，不派游荡
	robots := []state.Robot{
		online("i1", 10), ghosting("g1"), fighting("f1"), walking("w1", 10), hatching("h1", 6),
		waitTask, submit, stuck, off, {Account: "", Online: true},
	}
	got := roampool.PickIdle(robots, 10)
	if len(got) != 1 || got[0] != "i1" {
		t.Fatalf("只该挑出真正空闲的 i1，实际 %v", got)
	}
}

func TestPickIdleRespectsN(t *testing.T) {
	robots := []state.Robot{online("i1", 10), online("i2", 10), online("i3", 10)}
	if got := roampool.PickIdle(robots, 2); join(got) != "i1,i2" {
		t.Fatalf("应按上限取 2 个且保序，实际 %v", got)
	}
	if got := roampool.PickIdle(robots, 0); len(got) != 0 {
		t.Fatalf("n<=0 应返回空，实际 %v", got)
	}
}

// ---------------------------------------------------------------- 纯函数：BalanceAssign

func groupSizes(g map[int][]string) map[int]int {
	out := map[int]int{}
	for k, v := range g {
		out[k] = len(v)
	}
	return out
}

func totalAssigned(g map[int][]string) int {
	n := 0
	for _, v := range g {
		n += len(v)
	}
	return n
}

func TestBalanceAssignEven(t *testing.T) {
	loads := []roampool.MapLoad{{MapID: 10}, {MapID: 26}, {MapID: 24}}
	accs := []string{"a1", "a2", "a3", "a4", "a5", "a6"}
	got := roampool.BalanceAssign(accs, loads, 6)
	sizes := groupSizes(got)
	if totalAssigned(got) != 6 {
		t.Fatalf("6 个号都要派出去，实际 %v", sizes)
	}
	for _, m := range []int{10, 24, 26} {
		if sizes[m] != 2 {
			t.Fatalf("3 张空图 6 个号应各 2 个，实际 %v", sizes)
		}
	}
}

func TestBalanceAssignRemainder(t *testing.T) {
	loads := []roampool.MapLoad{{MapID: 10}, {MapID: 24}, {MapID: 26}}
	got := roampool.BalanceAssign([]string{"a1", "a2", "a3", "a4", "a5"}, loads, 5)
	sizes := groupSizes(got)
	if totalAssigned(got) != 5 {
		t.Fatalf("应派出 5 个，实际 %v", sizes)
	}
	if sizes[10] != 2 || sizes[24] != 2 || sizes[26] != 1 {
		t.Fatalf("余数 1 个应落在图号最大的那张空图（并列时图号小的先满），实际 %v", sizes)
	}
}

func TestBalanceAssignMoreMapsThanAccounts(t *testing.T) {
	loads := []roampool.MapLoad{{MapID: 6}, {MapID: 10}, {MapID: 17}, {MapID: 24}, {MapID: 26}}
	got := roampool.BalanceAssign([]string{"a1", "a2"}, loads, 2)
	sizes := groupSizes(got)
	if len(sizes) != 2 || sizes[6] != 1 || sizes[10] != 1 {
		t.Fatalf("图比号多时应各占 1 张图（图号小的先派），实际 %v", sizes)
	}
	if totalAssigned(got) != 2 {
		t.Fatalf("只该派 2 个，实际 %v", sizes)
	}
}

func TestBalanceAssignRespectsExistingLoad(t *testing.T) {
	// 图10 已 5 人、图24/26 各 1 人：4 个号应全给人少的图（10 再加只会更高）
	loads := []roampool.MapLoad{{MapID: 10, Count: 5}, {MapID: 26, Count: 1}, {MapID: 24, Count: 1}}
	got := roampool.BalanceAssign([]string{"a1", "a2", "a3", "a4"}, loads, 4)
	sizes := groupSizes(got)
	if sizes[10] != 0 || sizes[24] != 2 || sizes[26] != 2 {
		t.Fatalf("应把号派给人少的图24/26（派完才 5/3/3，最大人数不升），实际 %v", sizes)
	}
}

func TestBalanceAssignEdges(t *testing.T) {
	loads := []roampool.MapLoad{{MapID: 10}}
	if got := roampool.BalanceAssign(nil, loads, 3); len(got) != 0 {
		t.Fatalf("没有号应返回空，实际 %v", got)
	}
	if got := roampool.BalanceAssign([]string{"a1"}, nil, 1); len(got) != 0 {
		t.Fatalf("没有可用的图应返回空，实际 %v", got)
	}
	if got := roampool.BalanceAssign([]string{"a1"}, loads, 0); len(got) != 0 {
		t.Fatalf("n<=0 应返回空，实际 %v", got)
	}
}

func TestMapLoadsCountsOnlinePerMap(t *testing.T) {
	robots := []state.Robot{online("a1", 10), walking("a2", 10), online("a3", 26), ghosting("a4")}
	loads := roampool.MapLoads(robots, []int{26, 10, 17})
	if len(loads) != 3 {
		t.Fatalf("白名单 3 张图应给 3 行，实际 %v", loads)
	}
	if loads[0].MapID != 10 || loads[0].Count != 3 {
		t.Fatalf("图10 应有 3 个在线号（含抓鬼的 a4），实际 %+v", loads[0])
	}
	if loads[1].MapID != 17 || loads[1].Count != 0 {
		t.Fatalf("图17 没人应 0（优先被派到），实际 %+v", loads[1])
	}
	if loads[2].MapID != 26 || loads[2].Count != 1 {
		t.Fatalf("图26 应有 1 个，实际 %+v", loads[2])
	}
}

// ---------------------------------------------------------------- 一轮：Tick

type dispatchCall struct {
	accounts []string
	mapid    any
	mode     string
	minutes  int
}

type fakeDeps struct {
	robots  []state.Robot
	deficit int
	maps    []int
	now     time.Time // 假时钟（零值 = time.Now）；P1 在途/退避用例用它控制 Status 的判定时刻

	disp        []dispatchCall
	stopped     [][]string
	logs        []string
	dispatchErr map[int]error // 指定图的图号 → 故意让它失败
}

func (f *fakeDeps) deps() roampool.Deps {
	return roampool.Deps{
		Robots:  func() []state.Robot { return f.robots },
		Deficit: func() int { return f.deficit },
		Maps:    func() []int { return f.maps },
		Now: func() time.Time {
			if f.now.IsZero() {
				return time.Now()
			}
			return f.now
		},
		Dispatch: func(accounts []string, mapid any, mode string, minutes int) (int, error) {
			f.disp = append(f.disp, dispatchCall{accounts: accounts, mapid: mapid, mode: mode, minutes: minutes})
			if id, ok := mapid.(int); ok {
				if err := f.dispatchErr[id]; err != nil {
					return 0, err
				}
			}
			return len(accounts), nil
		},
		Stop: func(accounts []string) (int, error) {
			f.stopped = append(f.stopped, accounts)
			return len(accounts), nil
		},
		Log: func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	}
}

// newKeeper 造一个已启用参数的 keeper（参数落盘到临时目录，不碰真实 data/）。
func newKeeper(t *testing.T, f *fakeDeps, patch func(*roampool.Config)) *roampool.Keeper {
	t.Helper()
	k := roampool.New(filepath.Join(t.TempDir(), "roampool.json"), f.deps())
	cfg := roampool.DefaultConfig()
	cfg.Enabled = true
	cfg.IntervalSec = 60
	if patch != nil {
		patch(&cfg)
	}
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("设置参数失败: %v", err)
	}
	return k
}

func TestTickDoesNothingWhenDisabled(t *testing.T) {
	f := &fakeDeps{robots: []state.Robot{walking("w1", 10)}, deficit: 3, maps: []int{10, 26}}
	k := roampool.New(filepath.Join(t.TempDir(), "roampool.json"), f.deps()) // 默认 enabled=false
	if k.Tick(time.Now()) {
		t.Fatal("enabled=false 时不该有任何动作")
	}
	if len(f.stopped) != 0 || len(f.disp) != 0 {
		t.Fatalf("enabled=false 时不该下发/回收：stop=%v dispatch=%v", f.stopped, f.disp)
	}
}

func TestTickReclaimsBeforeDispatching(t *testing.T) {
	f := &fakeDeps{
		robots:  []state.Robot{walking("w1", 10), walking("w2", 10), online("i1", 10), online("i2", 26)},
		deficit: 3, maps: []int{10, 26},
	}
	k := newKeeper(t, f, nil)
	if !k.Tick(time.Now()) {
		t.Fatal("任务池缺人时应下发回收")
	}
	if len(f.stopped) != 1 || join(f.stopped[0]) != "w1,w2" {
		t.Fatalf("缺口 3 / 在游荡 2 → 两个游荡号都回收，实际 %v", f.stopped)
	}
	if len(f.disp) != 0 {
		t.Fatalf("回收轮不该同时补位（名额让给任务池），实际 %v", f.disp)
	}
	if !strings.Contains(k.Status().LastAction, "回收 2 个游荡号") {
		t.Fatalf("最近动作应说明回收了几个，实际 %q", k.Status().LastAction)
	}
}

func TestTickReclaimRespectsMaxStep(t *testing.T) {
	f := &fakeDeps{
		robots:  []state.Robot{walking("w1", 10), walking("w2", 10), walking("w3", 26), walking("w4", 26)},
		deficit: 10, maps: []int{10, 26},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.MaxStep = 2 })
	if !k.Tick(time.Now()) {
		t.Fatal("应下发回收")
	}
	if len(f.stopped) != 1 || len(f.stopped[0]) != 2 {
		t.Fatalf("单轮最多回收 max_step=2 个，实际 %v", f.stopped)
	}
}

func TestTickDeficitWithNoRoamersDoesNotDispatch(t *testing.T) {
	f := &fakeDeps{
		robots:  []state.Robot{online("i1", 10), online("i2", 26)},
		deficit: 2, maps: []int{10, 26},
	}
	k := newKeeper(t, f, nil)
	if k.Tick(time.Now()) {
		t.Fatal("没有游荡号可回收时不该有动作")
	}
	if len(f.disp) != 0 {
		t.Fatalf("任务池还缺人时不该补游荡（名额要留给任务池），实际 %v", f.disp)
	}
	if !strings.Contains(k.Status().LastAction, "没有可回收的游荡号") {
		t.Fatalf("应说明为什么没动作，实际 %q", k.Status().LastAction)
	}
}

func TestTickDispatchBalancedByMapLoad(t *testing.T) {
	// 5 个空闲号都在图99（非候选图，不参与负载）；候选图 10 已 4 人、26 已 2 人 →
	// 人少的图26 应分到更多（4 个号：26×3 + 10×1 → 派完 10=5 / 26=5，人数拉平）
	f := &fakeDeps{
		robots: []state.Robot{
			online("i1", 99), online("i2", 99), online("i3", 99), online("i4", 99),
			online("x1", 10), online("x2", 10), online("x3", 10), online("x4", 10),
			online("y1", 26), online("y2", 26),
		},
		deficit: 0, maps: []int{10, 26},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 4; c.Mode = "dense"; c.Minutes = 30 })
	if !k.Tick(time.Now()) {
		t.Fatal("余量号应被派去游荡")
	}
	if len(f.disp) != 2 {
		t.Fatalf("按图分配应每张图一条命令，实际 %v", f.disp)
	}
	byMap := map[int]dispatchCall{}
	for _, c := range f.disp {
		id, _ := c.mapid.(int)
		byMap[id] = c
		if c.mode != "dense" || c.minutes != 30 {
			t.Fatalf("档位/限时应按配置下发，实际 %+v", c)
		}
	}
	if len(byMap[26].accounts) != 3 || len(byMap[10].accounts) != 1 {
		t.Fatalf("人少的图26 应分到更多（26×3 / 10×1），实际 图10=%d 图26=%d",
			len(byMap[10].accounts), len(byMap[26].accounts))
	}
	if n := len(byMap[10].accounts) + len(byMap[26].accounts); n != 4 {
		t.Fatalf("本轮应派出 min(目标 4, 空闲 4, max_step 5)=4 个，实际 %d", n)
	}
	if !strings.Contains(k.Status().LastAction, "补位 4 个 → 图10×1 图26×3") {
		t.Fatalf("最近动作应带「补位 N 个 → 图x×n …」，实际 %q", k.Status().LastAction)
	}
}

func TestTickDispatchBalancedTieBreakByMapID(t *testing.T) {
	// 图10 只 1 人、图26 已 4 人 → 号全给人少的图10；第 4 个号时两图并列 4:4，
	// 并列取图号小的（10）→ 图10×4 / 图26×1（派完 5:5，人数拉平）
	f := &fakeDeps{
		robots: []state.Robot{
			online("i1", 99), online("i2", 99), online("i3", 99), online("i4", 99), online("i5", 99),
			online("x1", 10), online("y1", 26), online("y2", 26), online("y3", 26), online("y4", 26),
		},
		deficit: 0, maps: []int{10, 26},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 5 })
	if !k.Tick(time.Now()) {
		t.Fatal("应下发补位")
	}
	byMap := map[int]int{}
	for _, c := range f.disp {
		id, _ := c.mapid.(int)
		byMap[id] = len(c.accounts)
	}
	if byMap[10] != 4 || byMap[26] != 1 {
		t.Fatalf("图10 只有 1 人应分到更多号（10×4 / 26×1），实际 %v", byMap)
	}
}

func TestTickDispatchRandomWhenBalanceOff(t *testing.T) {
	f := &fakeDeps{
		robots:  []state.Robot{online("i1", 10), online("i2", 10)},
		deficit: 0, maps: []int{10, 26},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 2; c.Balance = false })
	if !k.Tick(time.Now()) {
		t.Fatal("应下发随机图游荡")
	}
	if len(f.disp) != 1 {
		t.Fatalf("balance=false 应一条命令下发所有号，实际 %v", f.disp)
	}
	if f.disp[0].mapid != "random" {
		t.Fatalf("balance=false 应走随机图（每号自抽），实际 mapid=%v", f.disp[0].mapid)
	}
	if len(f.disp[0].accounts) != 2 {
		t.Fatalf("应派 2 个号，实际 %v", f.disp[0].accounts)
	}
}

func TestTickStopsDispatchingAtTarget(t *testing.T) {
	f := &fakeDeps{
		robots:  []state.Robot{walking("w1", 10), walking("w2", 10), online("i1", 26)},
		deficit: 0, maps: []int{10, 26},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 2 })
	if k.Tick(time.Now()) {
		t.Fatal("已达标（在游荡 2 / 目标 2）不该再有动作")
	}
	if len(f.disp) != 0 {
		t.Fatalf("不该补位，实际 %v", f.disp)
	}
	if !strings.Contains(k.Status().LastAction, "游荡池达标") {
		t.Fatalf("应说明已达标，实际 %q", k.Status().LastAction)
	}
}

func TestTickSingleMapFailureDoesNotAbortOthers(t *testing.T) {
	f := &fakeDeps{
		robots:      []state.Robot{online("i1", 10), online("i2", 26)},
		deficit:     0,
		maps:        []int{10, 26},
		dispatchErr: map[int]error{10: errors.New("机器人通道未连接")},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 2 })
	if !k.Tick(time.Now()) {
		t.Fatal("另一张图成功也应算本轮有动作")
	}
	if len(f.disp) != 2 {
		t.Fatalf("两张图都该尝试下发，实际 %v", f.disp)
	}
	if !strings.Contains(strings.Join(f.logs, "\n"), "补位→图10失败") {
		t.Fatalf("失败应写日志（下一轮会再补），实际日志 %v", f.logs)
	}
	if !strings.Contains(k.Status().LastAction, "补位 1 个 → 图26×1") {
		t.Fatalf("动作文案只算**真的下发成功**的图（不虚报失败的图10），实际 %q", k.Status().LastAction)
	}
}

func TestTickWhitelistIntersectsGridMaps(t *testing.T) {
	f := &fakeDeps{
		robots:  []state.Robot{online("i1", 10)},
		deficit: 0,
		maps:    []int{10, 26}, // 链数据里有网格的图
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 1; c.Maps = []int{99, 26} })
	if !k.Tick(time.Now()) {
		t.Fatal("白名单里图26 有网格，应下发")
	}
	if len(f.disp) != 1 {
		t.Fatalf("应只对白名单∩有网格的图下发，实际 %v", f.disp)
	}
	if id, _ := f.disp[0].mapid.(int); id != 26 {
		t.Fatalf("不该派到没有网格的图 99，实际 %v", f.disp[0].mapid)
	}
}

// ---------------------------------------------------------------- 参数

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roampool.json")
	k := roampool.New(path, (&fakeDeps{}).deps())
	cfg := roampool.DefaultConfig()
	cfg.Enabled, cfg.Target, cfg.Maps, cfg.Balance = true, 42, []int{6, 17}, false
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("设置参数失败: %v", err)
	}
	k2 := roampool.New(path, (&fakeDeps{}).deps())
	if err := k2.Load(); err != nil {
		t.Fatalf("读回参数失败: %v", err)
	}
	got := k2.Config()
	if !got.Enabled || got.Target != 42 || got.Balance || len(got.Maps) != 2 ||
		got.Maps[0] != 6 || got.Maps[1] != 17 {
		t.Fatalf("参数应原样读回，实际 %+v", got)
	}
}

func TestConfigEnvOverridesFile(t *testing.T) {
	t.Setenv("CTRL_ROAMPOOL_TARGET", "7")
	t.Setenv("CTRL_ROAMPOOL_ENABLED", "1")
	t.Setenv("CTRL_ROAMPOOL_MAPS", "6, 17,34")
	path := filepath.Join(t.TempDir(), "roampool.json")
	k := roampool.New(path, (&fakeDeps{}).deps())
	cfg := roampool.DefaultConfig()
	cfg.Enabled, cfg.Target = false, 100
	if err := k.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	k2 := roampool.New(path, (&fakeDeps{}).deps())
	if err := k2.Load(); err != nil {
		t.Fatal(err)
	}
	got := k2.Config()
	if !got.Enabled || got.Target != 7 {
		t.Fatalf("环境变量应覆盖文件（enabled=1 target=7），实际 enabled=%v target=%d", got.Enabled, got.Target)
	}
	if len(got.Maps) != 3 || got.Maps[0] != 6 || got.Maps[2] != 34 {
		t.Fatalf("CTRL_ROAMPOOL_MAPS 应解析成 3 张图，实际 %v", got.Maps)
	}
	pinned := append([]string(nil), roampool.EnvPinned()...)
	sort.Strings(pinned)
	if join(pinned) != "enabled,maps,target" {
		t.Fatalf("应报告被环境变量固定的字段，实际 %v", pinned)
	}
}

func TestConfigValidateRejectsOutOfRange(t *testing.T) {
	k := roampool.New(filepath.Join(t.TempDir(), "roampool.json"), (&fakeDeps{}).deps())
	cases := []struct {
		name  string
		patch func(*roampool.Config)
	}{
		{"target 为负", func(c *roampool.Config) { c.Target = -1 }},
		{"间隔过小", func(c *roampool.Config) { c.IntervalSec = 5 }},
		{"单轮为 0", func(c *roampool.Config) { c.MaxStep = 0 }},
		{"限时为负", func(c *roampool.Config) { c.Minutes = -1 }},
		{"白名单图号非法", func(c *roampool.Config) { c.Maps = []int{0} }},
	}
	for _, cs := range cases {
		cfg := roampool.DefaultConfig()
		cs.patch(&cfg)
		if err := k.SetConfig(cfg); err == nil {
			t.Fatalf("%s 应被拒绝", cs.name)
		}
	}
	// 没通过校验就不该改内存里的参数（保持默认）
	if got := k.Config().Target; got != roampool.DefaultTarget {
		t.Fatalf("非法参数不该生效，实际 target=%d", got)
	}
}

// 2026-09-22 P0：回收资格闸 —— 回收后进不了任务池的号（今日满额/等级不够）不回收。
// 背景：原实现只判"在游荡"，把满额号也回收给任务池，任务池跳过它们 → 90s 后机器人端
// auto_roam 又派游荡 → 回收-重派来回损耗（实测单号 20+ 次往返）。eligible=nil 保持旧行为。
func TestPickReclaimEligibleGate(t *testing.T) {
	robots := []state.Robot{
		walking("full", 10), // 满额：eligible=false → 不该回收
		walking("ok", 10),   // 正常：该回收
	}
	got := roampool.PickReclaim(robots, 5, func(r state.Robot) bool { return r.Account == "ok" })
	if join(got) != "ok" {
		t.Fatalf("资格过滤后应只回收 ok，实际 %q", join(got))
	}
	all := roampool.PickReclaim(robots, 5, nil)
	if len(all) != 2 {
		t.Fatalf("eligible=nil 应全放行（2 个），实际 %d", len(all))
	}
	// 有资格的号不够 n：就有几个给几个（不硬凑）
	only := roampool.PickReclaim(robots, 2, func(r state.Robot) bool { return r.Account == "ok" })
	if join(only) != "ok" {
		t.Fatalf("不够数时应只给 ok，实际 %q", join(only))
	}
}

// ---------------------------------------------------------------- 2026-09-23 P1：超编温和收敛

// ghostAt 在抓鬼的号（ghost.enabled=true），指定当前状态。
func ghostAt(acc, st string) state.Robot {
	r := ghosting(acc)
	r.State = st
	return r
}

// 纯函数：只挑"在抓鬼 + 可中断"的号；战斗/交付/对话/跨图导航/游荡/孵化/离线一律不碰。
func TestPickExcessOnlyInterruptible(t *testing.T) {
	robots := []state.Robot{
		ghostAt("g1", "WAIT_GHOST"), ghostAt("g2", "READY"), ghostAt("g3", "IDLE"),
		ghostAt("f1", "FIGHT"), ghostAt("s1", "SUBMIT"), ghostAt("d1", "DIALOG"),
		ghostAt("n1", "NAV"), ghostAt("c1", "CLICK"), ghostAt("w1", "WAIT_NEXT"),
		walking("r1", 10), hatching("h1", 10),
		online("i1", 99), // 空闲但没在抓鬼：走正常补位，不算"超编回收"
	}
	if got := roampool.PickExcess(robots, 10); join(got) != "g1,g2,g3" {
		t.Fatalf("只该挑可中断的在抓鬼号（g1,g2,g3），实际 %s", join(got))
	}
	if extra := roampool.PickExcess(robots, 2); join(extra) != "g1,g2" {
		t.Fatalf("n 应截断（升序前 2 个），实际 %s", join(extra))
	}
	if got := roampool.PickExcess(robots, 0); got != nil {
		t.Fatalf("n<=0 应返回 nil，实际 %v", got)
	}
}

// 2026-09-23 R3（操作健壮性审计修复）：人工暂停（面板点过「停止」）的号 ——
// 不派游荡、不从抓鬼转游荡、不回收（留着等用户自己决定）。
func TestPausedSkippedInPicks(t *testing.T) {
	pIdle := online("p1", 10)
	pIdle.Paused = true
	if got := roampool.PickIdle([]state.Robot{pIdle, online("i1", 10)}, 10); join(got) != "i1" {
		t.Fatalf("暂停号不该被派游荡: %v", got)
	}

	pGhost := ghostAt("pg1", "WAIT_GHOST")
	pGhost.Paused = true
	if got := roampool.PickExcess([]state.Robot{pGhost, ghostAt("g1", "WAIT_GHOST")}, 10); join(got) != "g1" {
		t.Fatalf("暂停号不该被从抓鬼转游荡: %v", got)
	}

	pWalk := walking("pw1", 10)
	pWalk.Paused = true
	if got := roampool.PickReclaim([]state.Robot{pWalk, walking("w1", 10)}, 10, nil); join(got) != "w1" {
		t.Fatalf("暂停号不该被回收: %v", got)
	}
}

// 任务池超编（deficit<0）+ 游荡不足 + 没有空闲号 → 从可中断的超编号里补位；
// 动作文案要说清"含超编回收 N 个"（可追溯）。
func TestTickExcessReclaimWhenOverDeficit(t *testing.T) {
	f := &fakeDeps{
		robots: []state.Robot{
			ghostAt("g1", "WAIT_GHOST"), ghostAt("g2", "WAIT_GHOST"), ghostAt("g3", "READY"),
			ghostAt("busy1", "FIGHT"), ghostAt("busy2", "SUBMIT"), ghostAt("busy3", "NAV"),
			walking("w1", 10), walking("w2", 26),
		},
		deficit: -8, maps: []int{10, 26},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 5 })
	if !k.Tick(time.Now()) {
		t.Fatal("任务池超编且游荡不足 → 应把可中断的超编号转游荡")
	}
	var got []string
	for _, c := range f.disp {
		got = append(got, c.accounts...)
	}
	sort.Strings(got)
	if join(got) != "g1,g2,g3" {
		t.Fatalf("应只回收可中断的 g1,g2,g3（busy/游荡号不碰），实际 %s", join(got))
	}
	if !strings.Contains(k.Status().LastAction, "含超编回收 3 个") {
		t.Fatalf("动作文案应说明超编回收，实际 %q", k.Status().LastAction)
	}
}

// 未超编（deficit>=0）时即使游荡不足、没有空闲号，也**不许**动抓鬼号。
func TestTickNoExcessReclaimWhenNotOverDeficit(t *testing.T) {
	f := &fakeDeps{
		robots:  []state.Robot{ghostAt("g1", "WAIT_GHOST"), ghostAt("g2", "WAIT_GHOST"), walking("w1", 10)},
		deficit: 0, maps: []int{10, 26},
	}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 5 })
	if k.Tick(time.Now()) || len(f.disp) != 0 {
		t.Fatalf("任务池不超编 → 不该回收抓鬼号，实际下发 %v", f.disp)
	}
	if !strings.Contains(k.Status().LastAction, "没有空闲号/可中断的超编号可派") {
		t.Fatalf("应说明没有可派号，实际 %q", k.Status().LastAction)
	}
}

// 节奏：每轮 ≤ MaxStep（用户口径"少量、别激进批量"）。
func TestTickExcessRespectsMaxStep(t *testing.T) {
	robots := []state.Robot{walking("w1", 10)}
	for i := 1; i <= 9; i++ {
		robots = append(robots, ghostAt(fmt.Sprintf("g%d", i), "WAIT_GHOST"))
	}
	f := &fakeDeps{robots: robots, deficit: -50, maps: []int{10, 26}}
	k := newKeeper(t, f, func(c *roampool.Config) { c.Target = 10; c.MaxStep = 2 })
	if !k.Tick(time.Now()) {
		t.Fatal("应下发超编回收")
	}
	var got []string
	for _, c := range f.disp {
		got = append(got, c.accounts...)
	}
	if len(got) != 2 {
		t.Fatalf("每轮最多 MaxStep=2 个，实际 %d（%v）", len(got), got)
	}
}
