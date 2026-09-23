// waterline 用例：纯决策（ComputeAdjust / PickOffline / PickOnlineCandidates / Busy）+ 一轮决策
// Tick（死区 / 限幅 / 兜底口径 / 在途记账 / 待下线 / min_keep）+ 参数落盘。
//
// 全部用假依赖（不联网、不起进程、不碰真实 data/）：断言的是**结果证据**——
// 下发了哪些账号、待下线/在途队列内容、来源与新鲜度、落盘文件内容。
package waterline_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 假依赖

type fake struct {
	now     time.Time
	robots  []state.Robot
	svr     int
	svrTSms float64
	svrOK   bool
	local   int
	cands   []waterline.Candidate

	onlineCalls  [][]string
	offlineCalls [][]string
	onlineOK     bool
	offlineOK    bool
	logs         []string
}

func newFake() *fake {
	return &fake{
		now:       time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		onlineOK:  true,
		offlineOK: true,
	}
}

func (f *fake) deps() waterline.Deps {
	return waterline.Deps{
		Now: func() time.Time { return f.now },
		Rand: func(n int) int { // 固定 0：打乱顺序可预期（i 与 0 交换）
			return 0
		},
		Robots:    func() []state.Robot { return f.robots },
		SvrOnline: func() (int, float64, bool) { return f.svr, f.svrTSms, f.svrOK },
		Local:     func() int { return f.local },
		// 候选 = 号池 + 运行时状态（与壳层 waterlineCandidates 同口径：在线/在忙跟着状态走）
		Candidates: func() []waterline.Candidate {
			out := make([]waterline.Candidate, 0, len(f.cands))
			for _, c := range f.cands {
				if r, ok := f.robotOf(c.Account); ok {
					c.Online = r.Online
					c.Busy = waterline.Busy(r)
				}
				out = append(out, c)
			}
			return out
		},
		Online: func(accs []string) ([]string, error) {
			f.onlineCalls = append(f.onlineCalls, accs)
			if !f.onlineOK {
				return nil, errors.New("机器人通道未连接")
			}
			// 真实壳层只下发命令，号要过几十秒才登录上报 → 这里**不**改 robots（由用例自己模拟登录）
			return accs, nil
		},
		Offline: func(accs []string) ([]string, error) {
			f.offlineCalls = append(f.offlineCalls, accs)
			if !f.offlineOK {
				return nil, errors.New("机器人通道未连接")
			}
			// 真实壳层会把号标记移除 + 从状态表删行 → 本地立刻不再在线
			f.markOffline(accs)
			return accs, nil
		},
		Log: func(format string, args ...any) {
			f.logs = append(f.logs, fmt.Sprintf(format, args...))
		},
	}
}

func (f *fake) robotOf(acc string) (state.Robot, bool) {
	for _, r := range f.robots {
		if r.Account == acc {
			return r, true
		}
	}
	return state.Robot{}, false
}

func (f *fake) markOffline(accs []string) {
	drop := map[string]bool{}
	for _, a := range accs {
		drop[a] = true
	}
	out := f.robots[:0]
	for _, r := range f.robots {
		if drop[r.Account] {
			r.Online = false
		}
		out = append(out, r)
	}
	f.robots = out
}

// online 一个在线号（state="ONLINE" 空闲）。
func online(acc string) state.Robot {
	return state.Robot{Account: acc, Online: true, State: "ONLINE", HS: true}
}

// ghosting 一个正在抓鬼的号（活跃会话 → Busy）。
func ghosting(acc string) state.Robot {
	return state.Robot{Account: acc, Online: true, State: "FIGHT", HS: true,
		Ghost: map[string]any{"enabled": true}}
}

// cand 一个可用候选。
func cand(acc string) waterline.Candidate {
	return waterline.Candidate{Account: acc, Usable: true}
}

// ---------------------------------------------------------------- ComputeAdjust

func TestComputeAdjustDeadZone(t *testing.T) {
	const (
		dz = 3
		ms = 5
	)
	if got := waterline.ComputeAdjust(100, 100, dz, ms); got != 0 {
		t.Fatalf("读数等于目标应不动: %d", got)
	}
	if got := waterline.ComputeAdjust(102, 100, dz, ms); got != 0 {
		t.Fatalf("|diff|=2 < 死区 3 应不动（压号侧）: %d", got)
	}
	if got := waterline.ComputeAdjust(98, 100, dz, ms); got != 0 {
		t.Fatalf("|diff|=2 < 死区 3 应不动（补号侧）: %d", got)
	}
	if got := waterline.ComputeAdjust(97, 100, dz, ms); got != 3 {
		t.Fatalf("|diff|=3 到死区边界应补 3 个: %d", got)
	}
	if got := waterline.ComputeAdjust(103, 100, dz, ms); got != -3 {
		t.Fatalf("|diff|=3 到死区边界应压 3 个: %d", got)
	}
	if got := waterline.ComputeAdjust(100, 100, 0, ms); got != 0 {
		t.Fatalf("死区 0 且无差应不动: %d", got)
	}
	if got := waterline.ComputeAdjust(100, 101, 0, ms); got != 1 {
		t.Fatalf("死区 0 时差 1 也要动: %d", got)
	}
}

func TestComputeAdjustClampAndGuards(t *testing.T) {
	if got := waterline.ComputeAdjust(0, 100, 3, 5); got != 5 {
		t.Fatalf("缺 100 个也只能补 maxStep=5: %d", got)
	}
	if got := waterline.ComputeAdjust(10, 100, 3, 5); got != 5 {
		t.Fatalf("缺 90 个也只能补 5: %d", got)
	}
	if got := waterline.ComputeAdjust(500, 100, 3, 5); got != -5 {
		t.Fatalf("超 400 个也只能压 5: %d", got)
	}
	if got := waterline.ComputeAdjust(100, 0, 3, 5); got != -5 {
		t.Fatalf("目标 0（全断）应压 -maxStep: %d", got)
	}
	if got := waterline.ComputeAdjust(0, 0, 3, 5); got != 0 {
		t.Fatalf("目标 0 且已无人应不动: %d", got)
	}
	if got := waterline.ComputeAdjust(-3, 0, 3, 5); got != 3 {
		t.Fatalf("差 3 = 死区边界（|diff|<3 不动）应补 3: %d", got)
	}
	if got := waterline.ComputeAdjust(0, 100, 3, 0); got != 0 {
		t.Fatalf("maxStep<=0（配置坏了）应不动: %d", got)
	}
	if got := waterline.ComputeAdjust(0, 100, 3, -5); got != 0 {
		t.Fatalf("maxStep 负数应不动: %d", got)
	}
	if got := waterline.ComputeAdjust(0, 10, -1, 5); got != 5 {
		t.Fatalf("死区负数按 0 处理，限幅仍生效: %d", got)
	}
}

// ---------------------------------------------------------------- PickOffline

func TestPickOfflinePrefersIdle(t *testing.T) {
	robots := []state.Robot{ghosting("busy1"), online("idle1"), online("idle2"), online("idle3")}
	now, pending := waterline.PickOffline(robots, 2, true)
	if len(now) != 2 || now[0] != "idle1" || now[1] != "idle2" {
		t.Fatalf("应优先断空闲号（按输入顺序）: now=%v", now)
	}
	if len(pending) != 0 {
		t.Fatalf("空闲号够时不该有忙号进待下线: %v", pending)
	}
}

func TestPickOfflineBusyGoesPending(t *testing.T) {
	robots := []state.Robot{ghosting("busy1"), online("idle1"), online("idle2")}
	now, pending := waterline.PickOffline(robots, 3, true)
	if len(now) != 2 || now[0] != "idle1" || now[1] != "idle2" {
		t.Fatalf("2 个空闲号应先进 now: now=%v", now)
	}
	if len(pending) != 1 || pending[0] != "busy1" {
		t.Fatalf("忙号（抓鬼中）只能进待下线、绝不硬断: pending=%v", pending)
	}
}

func TestPickOfflineNotEnoughAndOfflineSkipped(t *testing.T) {
	robots := []state.Robot{
		{Account: "offline1"}, // 已离线：不参与
		online("idle1"),
		ghosting("busy1"),
	}
	now, pending := waterline.PickOffline(robots, 5, true)
	if len(now) != 1 || now[0] != "idle1" {
		t.Fatalf("不足时也只给 1 个空闲号: now=%v", now)
	}
	if len(pending) != 1 || pending[0] != "busy1" {
		t.Fatalf("不足时忙号进待下线: pending=%v", pending)
	}
	if len(now)+len(pending) != 2 {
		t.Fatalf("离线号不能被选中（总选中数=2）: now=%v pending=%v", now, pending)
	}
	if n, p := waterline.PickOffline(robots, 0, true); n != nil || p != nil {
		t.Fatalf("n<=0 应返回空: %v %v", n, p)
	}
}

func TestPickOfflineWithoutIdlePreference(t *testing.T) {
	robots := []state.Robot{ghosting("busy1"), online("idle1"), online("idle2")}
	now, pending := waterline.PickOffline(robots, 2, false)
	if len(pending) != 1 || pending[0] != "busy1" {
		t.Fatalf("不挑空闲也绝不硬断忙号: pending=%v", pending)
	}
	if len(now) != 1 || now[0] != "idle1" {
		t.Fatalf("按输入顺序取（忙碌的先进 pending）: now=%v", now)
	}
}

// 2026-09-23（与地图页 26f4660 同口径）：交付中（SUBMIT）与卡住（ERROR）的号都不能
// 当"空闲号"立刻压 —— 前者在推进、后者是异常（等人工/机器人端自愈）；两者都只进"待下线"。
func TestPickOfflineSubmitAndErrorGoPending(t *testing.T) {
	robots := []state.Robot{
		online("idle1"),
		{Account: "sub1", Online: true, State: "SUBMIT"},
		{Account: "err1", Online: true, State: "ERROR"},
	}
	now, pending := waterline.PickOffline(robots, 3, true)
	if len(now) != 1 || now[0] != "idle1" {
		t.Fatalf("只有真空闲号能立刻压: now=%v", now)
	}
	if len(pending) != 2 || pending[0] != "sub1" || pending[1] != "err1" {
		t.Fatalf("SUBMIT（推进中）/ ERROR（异常）只进待下线: pending=%v", pending)
	}
	// 不挑空闲（prefer_idle=false）也一样：忙/异常号绝不进 now
	now2, pending2 := waterline.PickOffline(robots, 3, false)
	if len(now2) != 1 || now2[0] != "idle1" {
		t.Fatalf("prefer_idle=false 时 now 仍只含空闲号: now=%v", now2)
	}
	if len(pending2) != 2 {
		t.Fatalf("prefer_idle=false 时 SUBMIT/ERROR 仍进待下线: pending=%v", pending2)
	}
}

func TestBusyJudgement(t *testing.T) {
	if waterline.Busy(online("a")) {
		t.Fatal("ONLINE 空闲号不该判为忙")
	}
	if !waterline.Busy(ghosting("a")) {
		t.Fatal("活跃抓鬼会话应判为忙")
	}
	if !waterline.Busy(state.Robot{Account: "a", Online: true, Fight: true}) {
		t.Fatal("战斗中应判为忙")
	}
	if !waterline.Busy(state.Robot{Account: "a", Online: true, State: "DIALOG"}) {
		t.Fatal("对话中（DIALOG）应判为忙")
	}
	if !waterline.Busy(state.Robot{Account: "a", Online: true, State: "WAIT_TASK", TaskIndex: 3}) {
		t.Fatal("任务链跑着（WAIT_TASK+任务索引）应判为忙")
	}
	if waterline.Busy(state.Robot{Account: "a", Online: true, State: "WAIT_TASK", TaskIndex: 0}) {
		t.Fatal("WAIT_TASK 且任务索引 0 = 收工态，不算忙")
	}
	if !waterline.Busy(state.Robot{Account: "a", Online: true, Walk: map[string]any{"enabled": true}}) {
		t.Fatal("游荡中应判为忙（游荡与抓鬼互斥，别打断）")
	}
	// 2026-09-23（前端 26f4660 同口径）：SUBMIT = 推进中（与 FIGHT/NAV 并列），ERROR = 异常。
	if !waterline.Busy(state.Robot{Account: "a", Online: true, State: "SUBMIT"}) {
		t.Fatal("交付中（SUBMIT）应判为忙：推进中，不压、不当空闲派活")
	}
	if !waterline.Busy(state.Robot{Account: "a", Online: true, State: " error "}) {
		t.Fatal("卡住（ERROR）应判为忙/异常（大小写与空白也要归一）：不派活、不硬压")
	}
	// 对照：等待段（WAIT_GHOST）不是忙 —— 与 roampool.Interruptible 的白名单口径一致
	// （有活跃抓鬼会话时 GhostActive 已经把它判成忙，这里是"没有会话、只在等推送"的形态）。
	if waterline.Busy(state.Robot{Account: "a", Online: true, State: "WAIT_GHOST"}) {
		t.Fatal("WAIT_GHOST 且无活跃抓鬼会话 = 等待段，不算忙（否则水位永远压不动等刷鬼的号）")
	}
	if !waterline.Busy(state.Robot{Account: "a", Online: true, State: "WAIT_GHOST",
		Ghost: map[string]any{"enabled": true}}) {
		t.Fatal("WAIT_GHOST 且抓鬼会话活跃 = 在忙（GhostActive 兜住）")
	}
}

// ---------------------------------------------------------------- PickOnlineCandidates

func TestPickOnlineCandidatesFilters(t *testing.T) {
	pool := []waterline.Candidate{
		cand("b-usable"),
		{Account: "a-unusable", Usable: false},
		{Account: "c-removed", Usable: true, Removed: true},
		{Account: "d-online", Usable: true, Online: true},
		{Account: "e-busy", Usable: true, Busy: true},
		cand("f-usable"),
		cand("g-usable"),
		{Account: "", Usable: true},
	}
	got := waterline.PickOnlineCandidates(pool, 2)
	if len(got) != 2 || got[0] != "b-usable" || got[1] != "f-usable" {
		t.Fatalf("应按账号升序取前 2 个可用候选: %v", got)
	}
	all := waterline.PickOnlineCandidates(pool, 10)
	if len(all) != 3 || all[0] != "b-usable" || all[1] != "f-usable" || all[2] != "g-usable" {
		t.Fatalf("不可用/已移除/已在线/在忙的号都不能拉: %v", all)
	}
	if n := waterline.PickOnlineCandidates(pool, 0); n != nil {
		t.Fatalf("n<=0 应返回空: %v", n)
	}
}

// 2026-09-23 R3（操作健壮性审计修复）：人工暂停（面板点过「停止」）的号 ——
// 水位保持器既不自动拉起、也不自动压号（别动用户明确停过的号）。
func TestPickSkipsPaused(t *testing.T) {
	pool := []waterline.Candidate{
		{Account: "a-ok", Usable: true},
		{Account: "b-paused", Usable: true, Paused: true},
		{Account: "c-ok", Usable: true},
	}
	got := waterline.PickOnlineCandidates(pool, 10)
	if len(got) != 2 || got[0] != "a-ok" || got[1] != "c-ok" {
		t.Fatalf("暂停号不能被自动拉起（补号候选应排除）: %v", got)
	}

	pIdle := online("p-idle")
	pIdle.Paused = true
	pBusy := ghosting("p-busy")
	pBusy.Paused = true
	robots := []state.Robot{online("i1"), pIdle, ghosting("b1"), pBusy}
	now, pending := waterline.PickOffline(robots, 3, true)
	if len(now) != 1 || now[0] != "i1" {
		t.Fatalf("暂停的空闲号不该被断: now=%v", now)
	}
	if len(pending) != 1 || pending[0] != "b1" {
		t.Fatalf("只有非暂停的忙号能进待下线（暂停号既不 now 也不 pending）: pending=%v", pending)
	}
	// preferIdle=false（随机补选）同样跳过暂停号
	now2, pending2 := waterline.PickOffline(robots, 3, false)
	if len(now2)+len(pending2) != 2 {
		t.Fatalf("非空闲优先模式下也只能选到 2 个非暂停号: now=%v pending=%v", now2, pending2)
	}
}

// ---------------------------------------------------------------- Tick（一轮决策）

func TestTickDisabledDoesNothing(t *testing.T) {
	f := newFake()
	f.local = 10
	f.cands = []waterline.Candidate{cand("a1"), cand("a2")}
	f.robots = []state.Robot{online("a1"), online("a2")}
	k := waterline.New(filepath.Join(t.TempDir(), "waterline.json"), f.deps())
	// 默认 enabled=false，即使差得很远也不许动作
	if acted := k.Tick(f.now); acted {
		t.Fatal("enabled=false 时不该有任何动作")
	}
	if len(f.onlineCalls)+len(f.offlineCalls) != 0 {
		t.Fatalf("enabled=false 时不该下发命令: %v %v", f.onlineCalls, f.offlineCalls)
	}
}

func TestTickSourceSvrThenLocalFallback(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.local = 7
	k := waterline.New(filepath.Join(dir, "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig()
	cfg.Enabled = true
	cfg.AllowLocal = true // 2026-09-22 安全闸: 本地口径用例需显式开
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	// ① 服务端读数新鲜 → 用它，source=svr、fresh=true
	f.svr, f.svrOK = 100, true
	f.svrTSms = float64(f.now.UnixMilli())
	st := k.Status()
	if st.Cur != 100 || st.Source != waterline.SourceSvr || !st.Fresh {
		t.Fatalf("新鲜的服务端读数应被采用: %+v", st)
	}
	if st.Diff != 0 || st.SvrAgeSec != 0 {
		t.Fatalf("目标 100 与读数 100 应差值 0、年龄 0: diff=%d age=%d", st.Diff, st.SvrAgeSec)
	}

	// ② 读数过期（>180s）→ 本地兜底，source=local、fresh=false（面板据此标注）
	f.svrTSms = float64(f.now.Add(-10 * time.Minute).UnixMilli())
	st = k.Status()
	if st.Cur != 7 || st.Source != waterline.SourceLocal || st.Fresh {
		t.Fatalf("过期读数应用本地握手数兜底: %+v", st)
	}
	if st.Svr != 100 || st.SvrAgeSec != 600 {
		t.Fatalf("过期的服务端读数仍要展示出来供排查: svr=%d age=%d", st.Svr, st.SvrAgeSec)
	}

	// ③ 完全没有读数 → 本地兜底
	f.svrOK = false
	st = k.Status()
	if st.Cur != 7 || st.Source != waterline.SourceLocal || st.SvrAgeSec != -1 {
		t.Fatalf("没有读数时应用本地兜底（age=-1）: %+v", st)
	}
}

func TestTickTopUpWhenBelowTarget(t *testing.T) {
	// 2026-09-22 安全闸: 本用例走"本地口径"(无 svr 读数) → 显式开 AllowLocal
	dir := t.TempDir()
	f := newFake()
	f.local = 3
	f.cands = []waterline.Candidate{cand("a1"), cand("a2"), cand("a3"), cand("a4"), cand("a5"), cand("a6")}
	k := waterline.New(filepath.Join(dir, "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig() // target 100、死区 3、单轮 5
	cfg.Enabled = true
	cfg.AllowLocal = true // 2026-09-22 安全闸: 本地口径用例需显式开
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	if acted := k.Tick(f.now); !acted {
		t.Fatal("缺 97 人应触发补号")
	}
	if len(f.onlineCalls) != 1 {
		t.Fatalf("一轮只补一次: %v", f.onlineCalls)
	}
	if got := f.onlineCalls[0]; len(got) != 5 || got[0] != "a1" || got[4] != "a5" {
		t.Fatalf("每轮最多补 maxStep=5（按账号升序取）: %v", got)
	}
	if st := k.Status(); len(st.InFlight) != 5 {
		t.Fatalf("补的号要记在途（防下一轮重复补）: %+v", st.InFlight)
	}

	// 在途号还没登录：读数没变 → 本轮不许再补（否则会把号池拉爆）
	f.onlineCalls = nil
	if acted := k.Tick(f.now.Add(60 * time.Second)); acted {
		t.Fatal("在途 5 个还没登录时不该再补")
	}
	if len(f.onlineCalls) != 0 {
		t.Fatalf("在途未落地不该重复下发: %v", f.onlineCalls)
	}

	// 在途号陆续登录（只有 a1 上线，a2~a5 还在途）→ 只补差额，别把号池拉爆
	f.robots = []state.Robot{online("a1")}
	f.local = 8
	f.onlineCalls = nil
	k.Tick(f.now.Add(120 * time.Second))
	if len(f.onlineCalls) != 1 || len(f.onlineCalls[0]) != 1 || f.onlineCalls[0][0] != "a6" {
		t.Fatalf("在途 4 个 + 已上线 1 个：本轮只该补 1 个未安排的号（a6）: %v", f.onlineCalls)
	}
}

func TestTickDeadZoneNoAction(t *testing.T) {
	f := newFake()
	f.local = 98 // target 100、死区 3 → |diff|=2 在死区内
	f.cands = []waterline.Candidate{cand("a1")}
	k := waterline.New(filepath.Join(t.TempDir(), "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig()
	cfg.Enabled = true
	_ = k.SetConfig(cfg)

	if acted := k.Tick(f.now); acted {
		t.Fatal("死区内不该动作")
	}
	if len(f.onlineCalls)+len(f.offlineCalls) != 0 {
		t.Fatalf("死区内不该下发: %v %v", f.onlineCalls, f.offlineCalls)
	}
	if st := k.Status(); st.Diff != 2 {
		t.Fatalf("差值应如实展示（面板要看）: %d", st.Diff)
	}
}

func TestTickOfflineIdleNowBusyPendingThenRelease(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.robots = []state.Robot{online("i1"), online("i2"), online("i3"), online("i4"), ghosting("b1")}
	f.local = 5
	// 服务端含真人读数 500：超目标 400 → 要压（远大于 maxStep，每轮只压 5）
	f.svr, f.svrOK, f.svrTSms = 500, true, float64(f.now.UnixMilli())
	k := waterline.New(filepath.Join(dir, "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig()
	cfg.Enabled = true
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	if acted := k.Tick(f.now); !acted {
		t.Fatal("远超目标应触发压号")
	}
	if len(f.offlineCalls) != 1 || len(f.offlineCalls[0]) != 4 {
		t.Fatalf("空闲号应立刻断（4 个）: %v", f.offlineCalls)
	}
	for _, a := range f.offlineCalls[0] {
		if strings.HasPrefix(a, "b") {
			t.Fatalf("正在抓鬼的号绝不能被硬断: %v", f.offlineCalls[0])
		}
	}
	st := k.Status()
	if len(st.Pending) != 1 || st.Pending[0] != "b1" {
		t.Fatalf("抓鬼中的号只能进待下线（不硬断）: %+v", st.Pending)
	}
	if len(f.offlineCalls[0])+len(st.Pending) != 5 {
		t.Fatalf("本轮动作量应 ≤ maxStep=5: offline=%d pending=%d", len(f.offlineCalls[0]), len(st.Pending))
	}

	// 下一轮：b1 收工（转空闲）→ 由"待下线"批次断掉；号池已空，不会再乱选
	b1 := f.robots[len(f.robots)-1]
	b1.State, b1.Ghost = "ONLINE", nil
	f.robots[len(f.robots)-1] = b1
	f.offlineCalls = nil
	k.Tick(f.now.Add(60 * time.Second))
	if len(f.offlineCalls) != 1 || len(f.offlineCalls[0]) != 1 || f.offlineCalls[0][0] != "b1" {
		t.Fatalf("待下线号收工后应被断掉: %v", f.offlineCalls)
	}
	if st := k.Status(); len(st.Pending) != 0 {
		t.Fatalf("断开后待下线应清空: %+v", st.Pending)
	}

	// 第三轮：没有可断的号了 → 安静记录，不报错、不重复下发
	f.offlineCalls = nil
	if acted := k.Tick(f.now.Add(120 * time.Second)); acted {
		t.Fatal("没号可断时不该算动作")
	}
	if len(f.offlineCalls) != 0 {
		t.Fatalf("没号可断时不该下发: %v", f.offlineCalls)
	}
	if k.Status().LastErr != "" {
		t.Fatalf("「没号可断」不是错误: %s", k.Status().LastErr)
	}
}

func TestTickSurplusRevokesPendingWhenBackInDeadZone(t *testing.T) {
	f := newFake()
	f.robots = []state.Robot{online("i1"), ghosting("b1")}
	f.local = 2
	f.svr, f.svrOK, f.svrTSms = 200, true, float64(f.now.UnixMilli())
	k := waterline.New(filepath.Join(t.TempDir(), "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig()
	cfg.Target, cfg.DeadZone, cfg.MaxStep = 10, 3, 5
	cfg.Enabled = true
	_ = k.SetConfig(cfg)

	k.Tick(f.now)
	st := k.Status()
	if len(st.Pending) != 1 {
		t.Fatalf("超目标时应把忙号标待下线: %+v", st.Pending)
	}
	// 真人们走了：读数落到目标附近（10）→ 撤销待下线，不许再多断号
	f.svr = 10
	f.offlineCalls = nil
	if acted := k.Tick(f.now.Add(60 * time.Second)); acted {
		t.Fatal("回到死区内不该再动作")
	}
	if n := len(k.Status().Pending); n != 0 {
		t.Fatalf("读数回目标附近应撤销待下线: %d", n)
	}
	if len(f.offlineCalls) != 0 {
		t.Fatalf("撤销后不该再下发下线: %v", f.offlineCalls)
	}
}

func TestTickMinKeepFloor(t *testing.T) {
	f := newFake()
	f.robots = []state.Robot{online("i1"), online("i2"), online("i3")}
	f.local = 3
	f.svr, f.svrOK, f.svrTSms = 300, true, float64(f.now.UnixMilli())
	k := waterline.New(filepath.Join(t.TempDir(), "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig()
	cfg.Target, cfg.DeadZone, cfg.MaxStep, cfg.MinKeep = 0, 3, 5, 2
	cfg.Enabled = true
	_ = k.SetConfig(cfg)

	k.Tick(f.now)
	// 目标 0 本会压 5 个；min_keep=2 只允许压到 2 个（本地握手 3 → 最多压 1）
	if len(f.offlineCalls) != 1 || len(f.offlineCalls[0]) != 1 {
		t.Fatalf("min_keep 保底应只压 1 个: %v", f.offlineCalls)
	}
}

// 单轮下发失败（通道断了）只记错误、不崩；下一轮还能继续。
func TestTickActionFailureKeepsLoopAlive(t *testing.T) {
	// 2026-09-22 安全闸: 同上, 显式开 AllowLocal
	f := newFake()
	f.local = 0
	f.cands = []waterline.Candidate{cand("a1")}
	f.onlineOK = false
	k := waterline.New(filepath.Join(t.TempDir(), "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig()
	cfg.Enabled = true
	cfg.AllowLocal = true // 2026-09-22 安全闸: 本地口径用例需显式开
	_ = k.SetConfig(cfg)

	if acted := k.Tick(f.now); acted {
		t.Fatal("下发失败不该算动作")
	}
	if !strings.Contains(k.Status().LastErr, "补号失败") {
		t.Fatalf("失败要记在状态里供面板看: %q", k.Status().LastErr)
	}
	f.onlineOK = true
	f.onlineCalls = nil
	if acted := k.Tick(f.now.Add(60 * time.Second)); !acted {
		t.Fatalf("下一轮应恢复正常: %v", f.onlineCalls)
	}
}

// ---------------------------------------------------------------- 参数与落盘

func TestConfigDefaultsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waterline.json")
	f := newFake()
	k := waterline.New(path, f.deps())
	if err := k.Load(); err != nil {
		t.Fatalf("首次 Load（没有文件）不该报错: %v", err)
	}
	d := k.Config()
	if d.Enabled {
		t.Fatal("默认必须 enabled=false（不自动动生产）")
	}
	if d.Target != 100 || d.DeadZone != 3 || d.MaxStep != 5 || d.IntervalSec != 60 || !d.PreferIdle {
		t.Fatalf("默认参数不符（目标 100/死区 3/单轮 5/间隔 60/空闲优先）: %+v", d)
	}

	want := waterline.Config{Enabled: true, Target: 150, DeadZone: 4, MaxStep: 7,
		IntervalSec: 90, PreferIdle: false, RandomFallback: false, MinKeep: 6}
	if err := k.SetConfig(want); err != nil {
		t.Fatalf("保存参数失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("参数应落盘 data/waterline.json: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("落盘内容不是 JSON: %v", err)
	}
	if onDisk["target"] != float64(150) || onDisk["enabled"] != true {
		t.Fatalf("落盘内容不符: %v", onDisk)
	}

	k2 := waterline.New(path, f.deps())
	if err := k2.Load(); err != nil {
		t.Fatalf("重启后 Load 失败: %v", err)
	}
	if got := k2.Config(); got != want {
		t.Fatalf("重启后参数应原样恢复: got=%+v want=%+v", got, want)
	}
}

func TestSetConfigRejectsOutOfRange(t *testing.T) {
	k := waterline.New(filepath.Join(t.TempDir(), "waterline.json"), newFake().deps())
	base := waterline.DefaultConfig()
	cases := []struct {
		name string
		cfg  waterline.Config
		hint string
	}{
		{"target 负", waterline.Config{Target: -1, DeadZone: 3, MaxStep: 5, IntervalSec: 60}, "target"},
		{"target 过大", waterline.Config{Target: 10001, DeadZone: 3, MaxStep: 5, IntervalSec: 60}, "target"},
		{"dead_zone 负", waterline.Config{Target: 100, DeadZone: -1, MaxStep: 5, IntervalSec: 60}, "dead_zone"},
		{"max_step 0", waterline.Config{Target: 100, DeadZone: 3, MaxStep: 0, IntervalSec: 60}, "max_step"},
		{"max_step 51", waterline.Config{Target: 100, DeadZone: 3, MaxStep: 51, IntervalSec: 60}, "max_step"},
		{"interval 9", waterline.Config{Target: 100, DeadZone: 3, MaxStep: 5, IntervalSec: 9}, "interval_sec"},
		{"interval 3601", waterline.Config{Target: 100, DeadZone: 3, MaxStep: 5, IntervalSec: 3601}, "interval_sec"},
		{"min_keep 负", waterline.Config{Target: 100, DeadZone: 3, MaxStep: 5, IntervalSec: 60, MinKeep: -1}, "min_keep"},
	}
	for _, c := range cases {
		err := k.SetConfig(c.cfg)
		if err == nil {
			t.Fatalf("%s 应被拒绝", c.name)
		}
		if !strings.Contains(err.Error(), c.hint) {
			t.Fatalf("%s 的错误信息应点名字段 %q: %v", c.name, c.hint, err)
		}
	}
	if k.Config() != base {
		t.Fatalf("校验失败不该改内存参数: %+v", k.Config())
	}
	// 合法值（含边界）必须放行
	for _, ok := range []waterline.Config{
		{Enabled: true, Target: 0, DeadZone: 0, MaxStep: 1, IntervalSec: 10},
		{Enabled: true, Target: 10000, DeadZone: 0, MaxStep: 50, IntervalSec: 3600},
	} {
		if err := k.SetConfig(ok); err != nil {
			t.Fatalf("边界值应放行 %+v: %v", ok, err)
		}
	}
}

// 部分残文件（手工改过/老版本）不许把 target 读成 0（那等于"把号全断"）。
func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waterline.json")
	if err := os.WriteFile(path, []byte("{\"enabled\": true}"), 0o644); err != nil {
		t.Fatal(err)
	}
	k := waterline.New(path, newFake().deps())
	if err := k.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	got := k.Config()
	if !got.Enabled {
		t.Fatal("文件里 enabled=true 应生效")
	}
	if got.Target != 100 || got.MaxStep != 5 || got.IntervalSec != 60 {
		t.Fatalf("缺字段要用默认值补齐（不许 target=0）: %+v", got)
	}
}

// 2026-09-22 安全闸用例：口径是"含真人总数"——服务端读数不可用时默认**不按本地数**调整。
func TestTickSkippedWhenSvrUnavailableByDefault(t *testing.T) {
	f := newFake()
	f.local = 3 // 本地只有 3 个（远低于 target 100）
	f.cands = []waterline.Candidate{cand("a1"), cand("a2"), cand("a3")}
	k := waterline.New(filepath.Join(t.TempDir(), "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig() // AllowLocal 默认 false
	cfg.Enabled = true
	_ = k.SetConfig(cfg)

	// ① 无 svr 读数 → 跳过（不下发），状态里说明原因
	if acted := k.Tick(f.now); acted {
		t.Fatal("无服务端读数时默认不该动作（会按本地口径超调）")
	}
	if len(f.onlineCalls)+len(f.offlineCalls) != 0 {
		t.Fatalf("不该下发: %v %v", f.onlineCalls, f.offlineCalls)
	}
	if !strings.Contains(k.Status().LastAction, "跳过") {
		t.Fatalf("应记录跳过原因: %q", k.Status().LastAction)
	}
	// ② 服务端读数新鲜 → 正常补号
	f.svr, f.svrOK = 3, true
	f.svrTSms = float64(f.now.UnixMilli())
	if acted := k.Tick(f.now); !acted {
		t.Fatal("服务端读数可用时应该补号")
	}
	// ③ 显式开 allow_local → 本地口径也动作（候选池要重新补，②已用掉）
	f.svrOK = false
	f.cands = []waterline.Candidate{cand("b1"), cand("b2")}
	cfg2 := k.Status()
	cfg3 := waterline.DefaultConfig()
	cfg3.Enabled = true
	cfg3.AllowLocal = true
	_ = cfg2
	_ = k.SetConfig(cfg3)
	if acted := k.Tick(f.now.Add(60 * time.Second)); !acted {
		t.Fatal("开了 allow_local 后本地口径应动作")
	}
}
