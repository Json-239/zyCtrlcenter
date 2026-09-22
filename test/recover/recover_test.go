// 恢复引擎 P1：按意图补发命令（start_chain / ghost_start），带冷却、重试上限、熔断。
//
// 口径对齐参考实现服务 intent_restore.py：
//
//	RESTORE_DELAY_SEC=8 / OFFLINE_READD_SEC=120 / READD_RETRY_SEC=300（失败重试）
//	重复错误 3 次封 600~1800s（REPULL_ERR_REPEAT=3 / REPULL_ERR_BLOCK_*）
//	补拉判据："在线 + task_index==0 + 没在跑" 才补（idle_wander.repull_task_for）
//
// 测试用假时钟驱动 Tick（不起 goroutine、不 sleep），可以反复跑验稳定。
package recover_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/restorer"
	"zyctrlcenter/internal/state"
)

type fake struct {
	now     time.Time
	up      bool
	enabled bool
	items   []intent.Intent
	robots  map[string]state.Robot
	repeat  map[string]int
	payload func(kind, chainID string) (any, error) // nil = 不带载荷（老行为）
	calls   []string                                // 记录 Payload 被问过谁（kind/chainID）
}

func (f *fake) deps() restorer.Deps {
	return restorer.Deps{
		Enabled:   func() bool { return f.enabled },
		ChannelUp: func() bool { return f.up },
		Intents:   func() []intent.Intent { return f.items },
		Robot:     func(a string) (state.Robot, bool) { r, ok := f.robots[a]; return r, ok },
		ErrRepeat: func(a string) int { return f.repeat[a] },
		Now:       func() time.Time { return f.now },
		Payload: func(kind, chainID string) (any, error) {
			f.calls = append(f.calls, kind+"/"+chainID)
			if f.payload == nil {
				return nil, nil
			}
			return f.payload(kind, chainID)
		},
	}
}

func newFake() *fake {
	return &fake{
		now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC), up: true, enabled: true,
		robots: map[string]state.Robot{}, repeat: map[string]int{},
	}
}

func online(account, st string, taskIndex int) state.Robot {
	return state.Robot{Account: account, Online: true, State: st, TaskIndex: taskIndex}
}

func TestTickStartsChainForIdleAccount(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "a@x.com", Kind: intent.KindNewbie, ChainID: "newbie_full"}}
	f.robots["a@x.com"] = online("a@x.com", "IDLE", 0)

	acts := restorer.New(f.deps()).Tick(f.now)
	if len(acts) != 1 {
		t.Fatalf("空闲账号应按意图补发一条命令: %+v", acts)
	}
	if acts[0].Command != "start_chain" || acts[0].ChainID != "newbie_full" {
		t.Fatalf("新手链意图应补发 start_chain(newbie_full): %+v", acts[0])
	}
}

func TestTickGhostUsesGhostStart(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "b@x.com", Kind: intent.KindGhost}}
	f.robots["b@x.com"] = online("b@x.com", "IDLE", 0)

	acts := restorer.New(f.deps()).Tick(f.now)
	if len(acts) != 1 || acts[0].Command != "ghost_start" {
		t.Fatalf("抓鬼意图应补发 ghost_start: %+v", acts)
	}
}

// 正在跑的别打扰；跑完的别重跑；WAIT_TASK 里有任务号说明链在推进 → 也别打扰。
func TestTickSkipsRunningDoneAndAdvancing(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "c@x.com", Kind: intent.KindNewbie, ChainID: "newbie_full"}}
	for _, st := range []string{"NAV", "CLICK", "DIALOG", "FIGHT", "SHOP", "ALLOC", "WAIT_NEXT", "DONE", "ERROR"} {
		f.robots["c@x.com"] = online("c@x.com", st, 7001003)
		if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 0 {
			t.Fatalf("状态 %s 不该被补发: %+v", st, acts)
		}
	}
	f.robots["c@x.com"] = online("c@x.com", "WAIT_TASK", 7001003)
	if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 0 {
		t.Fatalf("WAIT_TASK 且有任务号（链在推进）不该补发: %+v", acts)
	}
	// 但 WAIT_TASK 且没有任务号 = 任务丢了（参考实现的补拉判据）→ 要补
	f.robots["c@x.com"] = online("c@x.com", "WAIT_TASK", 0)
	if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 1 {
		t.Fatalf("WAIT_TASK 且 task_index=0 应补发: %+v", acts)
	}
}

func TestTickSkipsWhenDisabledChannelDownOrOffline(t *testing.T) {
	base := func() *fake {
		f := newFake()
		f.items = []intent.Intent{{Account: "d@x.com", Kind: intent.KindNewbie, ChainID: "newbie_full"}}
		f.robots["d@x.com"] = online("d@x.com", "IDLE", 0)
		return f
	}
	f := base()
	f.enabled = false
	if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 0 {
		t.Fatalf("开关关着不该发: %+v", acts)
	}
	f = base()
	f.up = false
	if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 0 {
		t.Fatalf("通道没连不该发: %+v", acts)
	}
	f = base()
	f.robots["d@x.com"] = state.Robot{Account: "d@x.com", Online: false, State: "IDLE"}
	if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 0 {
		t.Fatalf("不在线不该发（掉线补拉是另一条路）: %+v", acts)
	}
	f = base()
	f.items = []intent.Intent{{Account: "d@x.com", Kind: intent.KindIdle}}
	if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 0 {
		t.Fatalf("空闲意图不该发: %+v", acts)
	}
}

// 冷却 → 重试 → 到达上限熔断；熔断期内一直不发，过期后自动解除。
func TestTickCooldownRetryAndCircuitBreaker(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "e@x.com", Kind: intent.KindGhost}}
	f.robots["e@x.com"] = online("e@x.com", "IDLE", 0)
	r := restorer.New(f.deps()) // 默认 RetrySec=300 / MaxAttempts=3 / CircuitSec=1800

	if acts := r.Tick(f.now); len(acts) != 1 {
		t.Fatalf("第一次应补发: %+v", acts)
	}
	if acts := r.Tick(f.now); len(acts) != 0 {
		t.Fatal("同一时刻不该重复补发（冷却中）")
	}
	f.now = f.now.Add(299 * time.Second)
	if acts := r.Tick(f.now); len(acts) != 0 {
		t.Fatal("冷却未到不该补发")
	}
	f.now = f.now.Add(2 * time.Second) // 共 301s
	if acts := r.Tick(f.now); len(acts) != 1 {
		t.Fatalf("冷却到点应重试: %+v", acts)
	}
	f.now = f.now.Add(301 * time.Second)
	if acts := r.Tick(f.now); len(acts) != 1 {
		t.Fatalf("第三次应再试一次: %+v", acts)
	}
	// 三次用完 → 熔断
	f.now = f.now.Add(301 * time.Second)
	if acts := r.Tick(f.now); len(acts) != 0 {
		t.Fatalf("达到重试上限应熔断: %+v", acts)
	}
	st := r.Status()["e@x.com"]
	if !st.Blocked || st.BlockTill.IsZero() {
		t.Fatalf("应记录熔断与解除时间: %+v", st)
	}
	f.now = st.BlockTill.Add(time.Second)
	if acts := r.Tick(f.now); len(acts) != 1 {
		t.Fatalf("熔断到期应自动解除并重试: %+v", acts)
	}
}

// 补发之后机器人真的跑起来了 → 计数归零（说明恢复成功，别再熔断它）。
// 抓鬼补发必须带载荷：载荷可取 → 正常产出决策，且确实按 kind=ghost 去要过载荷。
func TestGhostRestoreCarriesPayload(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "h@x.com", Kind: intent.KindGhost}}
	f.robots["h@x.com"] = online("h@x.com", "IDLE", 0)
	f.payload = func(kind, chainID string) (any, error) {
		if kind != string(intent.KindGhost) {
			t.Fatalf("抓鬼补发应按 kind=ghost 取载荷: %q", kind)
		}
		return map[string]any{"ghost_maps": []any{9, 10}}, nil
	}

	acts := restorer.New(f.deps()).Tick(f.now)
	if len(acts) != 1 || acts[0].Command != "ghost_start" {
		t.Fatalf("载荷可取时应正常补发: %+v", acts)
	}
	if len(f.calls) == 0 || f.calls[0] != string(intent.KindGhost)+"/" {
		t.Fatalf("应带着意图种类去取载荷（供 api 决定给什么）: %v", f.calls)
	}
}

// 载荷取不到（导航数据缺失/改坏）：**不瞎发**，记一次失败并说明；连续到上限 → 熔断等人工。
func TestRestoreSkipsWhenPayloadMissing(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "i@x.com", Kind: intent.KindGhost}}
	f.robots["i@x.com"] = online("i@x.com", "IDLE", 0)
	f.payload = func(kind, chainID string) (any, error) {
		return nil, errors.New("链数据文件不存在: data/chains/zhongkui_nav.json")
	}
	r := restorer.New(f.deps())

	if acts := r.Tick(f.now); len(acts) != 0 {
		t.Fatalf("载荷缺失不该产出补发决策: %+v", acts)
	}
	st := r.Status()["i@x.com"]
	if st.Attempts != 1 || !strings.Contains(st.LastMsg, "载荷不可用") {
		t.Fatalf("应记一次失败并说明原因: %+v", st)
	}
	// 连续三次（每次过冷却）→ 熔断
	for i := 0; i < 2; i++ {
		f.now = f.now.Add(301 * time.Second)
		if acts := r.Tick(f.now); len(acts) != 0 {
			t.Fatalf("载荷一直缺失不该补发: %+v", acts)
		}
	}
	st = r.Status()["i@x.com"]
	if !st.Blocked || st.BlockTill.IsZero() {
		t.Fatalf("连续失败应熔断等人工: %+v", st)
	}
}

// 合并下发：同命令 + 同链的账号合成一条（不然抓鬼 2MB 载荷会被放大成 N 倍）。
func TestGroupActionsMergesByCommandAndChain(t *testing.T) {
	acts := []restorer.Action{
		{Account: "a@x.com", Kind: "ghost", Command: "ghost_start"},
		{Account: "b@x.com", Kind: "ghost", Command: "ghost_start"},
		{Account: "c@x.com", Kind: "newbie", Command: "start_chain", ChainID: "newbie_full"},
		// 同一条命令但链不同 → 不能合（载荷不一样）
		{Account: "d@x.com", Kind: "newbie", Command: "start_chain", ChainID: "zhuaogui"},
	}
	groups := restorer.GroupActions(acts)
	if len(groups) != 3 {
		t.Fatalf("应按（命令, 链）分成 3 组: %+v", groups)
	}
	if g := groups[0]; g.Command != "ghost_start" || len(g.Accounts) != 2 {
		t.Fatalf("两个抓鬼号应合成一条命令: %+v", g)
	}
	if g := groups[1]; g.Command != "start_chain" || g.ChainID != "newbie_full" || g.Accounts[0] != "c@x.com" {
		t.Fatalf("同链的 start_chain 应合并: %+v", g)
	}
	if g := groups[2]; g.ChainID != "zhuaogui" {
		t.Fatalf("不同链不能合并: %+v", g)
	}
	// 组内顺序稳定（acts 已按账号排序）
	if got := strings.Join(groups[0].Accounts, ","); got != "a@x.com,b@x.com" {
		t.Fatalf("组内账号应保持排序: %s", got)
	}
	// 同一个号重复决策（理论上不该有）→ 去重
	dup := restorer.GroupActions([]restorer.Action{
		{Account: "e@x.com", Command: "ghost_start"},
		{Account: "e@x.com", Command: "ghost_start"},
	})
	if len(dup) != 1 || len(dup[0].Accounts) != 1 {
		t.Fatalf("同一个号不该在一批里出现两次: %+v", dup)
	}
}

func TestTickResetsAttemptsOnceRunning(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "f@x.com", Kind: intent.KindNewbie, ChainID: "newbie_full"}}
	f.robots["f@x.com"] = online("f@x.com", "IDLE", 0)
	r := restorer.New(f.deps())
	r.Tick(f.now)
	if r.Status()["f@x.com"].Attempts != 1 {
		t.Fatalf("应记一次尝试: %+v", r.Status()["f@x.com"])
	}
	f.robots["f@x.com"] = online("f@x.com", "NAV", 0) // 跑起来了
	f.now = f.now.Add(10 * time.Second)
	if acts := r.Tick(f.now); len(acts) != 0 {
		t.Fatalf("在跑就不该再发: %+v", acts)
	}
	if got := r.Status()["f@x.com"].Attempts; got != 0 {
		t.Fatalf("跑起来后尝试计数应归零（恢复成功）: %d", got)
	}
}

// 错误重复过多（同错 ≥3 次）→ 直接熔断，等人工处理（参考实现：别再对着卡死的号猛补）。
func TestTickCircuitsOnRepeatedErrors(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "g@x.com", Kind: intent.KindNewbie, ChainID: "newbie_full"}}
	f.robots["g@x.com"] = online("g@x.com", "IDLE", 0)
	f.repeat["g@x.com"] = 3
	r := restorer.New(f.deps())
	if acts := r.Tick(f.now); len(acts) != 0 {
		t.Fatalf("错误重复 ≥3 不该补发: %+v", acts)
	}
	st := r.Status()["g@x.com"]
	if !st.Blocked || st.LastMsg == "" {
		t.Fatalf("应熔断并说明原因: %+v", st)
	}
}

// 2026-09-21 生产修复：抓鬼号"重登/上线后卡零选项空对话" —— state=DIALOG 且**没有
// 活跃抓鬼会话**（ghost.enabled 非 true：无字段或字段存在但已停都算）时恢复引擎应补发
// ghost_start（旧判据把 DIALOG 一律当"在跑"，autotask 候选/保持数/恢复引擎三道兜底全不管
// → 40+ 个号永久空转 12+ 分钟）。
// 有会话（正常钟馗对话）、有任务号（正推进）、新手链意图（重发会重置进度）都不补。
func TestTickRestoresGhostStuckInDialog(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{{Account: "k@x.com", Kind: intent.KindGhost}}
	f.robots["k@x.com"] = online("k@x.com", "DIALOG", 0) // 卡死形态
	if acts := restorer.New(f.deps()).Tick(f.now); len(acts) != 1 || acts[0].Command != "ghost_start" {
		t.Fatalf("抓鬼号卡 DIALOG 且无会话应补发 ghost_start: %+v", acts)
	}

	f2 := newFake()
	f2.items = []intent.Intent{{Account: "k@x.com", Kind: intent.KindGhost}}
	r := online("k@x.com", "DIALOG", 0)
	r.Ghost = map[string]any{"enabled": true, "state": "READY"} // 正常跟钟馗对话
	f2.robots["k@x.com"] = r
	if acts := restorer.New(f2.deps()).Tick(f2.now); len(acts) != 0 {
		t.Fatalf("有活跃抓鬼会话的 DIALOG 不该补发: %+v", acts)
	}

	f3 := newFake()
	f3.items = []intent.Intent{{Account: "k@x.com", Kind: intent.KindGhost}}
	f3.robots["k@x.com"] = online("k@x.com", "DIALOG", 2019508) // 任务在手
	if acts := restorer.New(f3.deps()).Tick(f3.now); len(acts) != 0 {
		t.Fatalf("DIALOG 且有任务号（正推进）不该补发: %+v", acts)
	}

	f4 := newFake()
	f4.items = []intent.Intent{{Account: "k@x.com", Kind: intent.KindNewbie, ChainID: "newbie_full"}}
	f4.robots["k@x.com"] = online("k@x.com", "DIALOG", 0)
	if acts := restorer.New(f4.deps()).Tick(f4.now); len(acts) != 0 {
		t.Fatalf("新手链意图的 DIALOG 不该补发（重发 start_chain 会重置进度）: %+v", acts)
	}

	// 2026-09-21 现场形态（15 个号）：ghost 字段还在但抓鬼会话**已停**（enabled=false，
	// IDLE 残留）—— 与"有活跃会话"不是一回事，同样该补（旧判据按"字段非空"会漏补）。
	f5 := newFake()
	f5.items = []intent.Intent{{Account: "k@x.com", Kind: intent.KindGhost}}
	r5 := online("k@x.com", "DIALOG", 0)
	r5.Ghost = map[string]any{"enabled": false, "state": "IDLE"}
	f5.robots["k@x.com"] = r5
	if acts := restorer.New(f5.deps()).Tick(f5.now); len(acts) != 1 || acts[0].Command != "ghost_start" {
		t.Fatalf("ghost 字段存在但已停（enabled=false）的 DIALOG 也应补发 ghost_start: %+v", acts)
	}
}

// 多账号：结果按账号排序（确定性）；没登记的账号不参与。
func TestTickDeterministicOrder(t *testing.T) {
	f := newFake()
	f.items = []intent.Intent{
		{Account: "z@x.com", Kind: intent.KindGhost},
		{Account: "a@x.com", Kind: intent.KindNewbie, ChainID: "newbie_full"},
	}
	f.robots["z@x.com"] = online("z@x.com", "IDLE", 0)
	f.robots["a@x.com"] = online("a@x.com", "IDLE", 0)

	r := restorer.New(f.deps())
	acts := r.Tick(f.now)
	if len(acts) != 2 || acts[0].Account != "a@x.com" || acts[1].Account != "z@x.com" {
		t.Fatalf("多账号补发应按账号排序: %+v", acts)
	}
	// 再跑 20 次（重新构造）结果一致
	for i := 0; i < 20; i++ {
		f2 := newFake()
		f2.items, f2.robots = f.items, f.robots
		acts2 := restorer.New(f2.deps()).Tick(f2.now)
		if len(acts2) != 2 || acts2[0].Account != "a@x.com" {
			t.Fatalf("第 %d 次结果不稳定: %+v", i, acts2)
		}
	}
}
