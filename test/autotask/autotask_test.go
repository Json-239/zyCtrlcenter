// autotask 引擎测试：随机间隔/随机批量、在线后才延迟下发、同时在线上限、没号自动注册、
// 停止后不再拉起（全是纯逻辑：假时钟 + 假依赖，不 sleep、不起 goroutine）。
package autotask_test

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
)

type fake struct {
	now        time.Time
	randVals   []int // 依次返回；用完就返回 0
	candidates map[autotask.Kind][]autotask.Candidate
	candCfg    map[autotask.Kind]autotask.Config // 记录回调收到的配置（合约：必须传本策略当前配置）
	onlineN    map[autotask.Kind]int
	onlineCall [][]string
	regCall    []int
	launchCall [][]string
}

func newFake() *fake {
	return &fake{
		now:        time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
		candidates: map[autotask.Kind][]autotask.Candidate{},
		candCfg:    map[autotask.Kind]autotask.Config{},
		onlineN:    map[autotask.Kind]int{},
	}
}

func (f *fake) deps() autotask.Deps {
	return autotask.Deps{
		Now: func() time.Time { return f.now },
		Rand: func(n int) int {
			if len(f.randVals) == 0 || n <= 0 {
				return 0
			}
			v := f.randVals[0]
			f.randVals = f.randVals[1:]
			if v >= n {
				v = n - 1
			}
			return v
		},
		Candidates: func(k autotask.Kind, cfg autotask.Config) []autotask.Candidate {
			f.candCfg[k] = cfg
			return f.candidates[k]
		},
		OnlineCount: func(k autotask.Kind) int { return f.onlineN[k] },
		Online: func(k autotask.Kind, accs []string) (int, error) {
			f.onlineCall = append(f.onlineCall, accs)
			f.onlineN[k] += len(accs)
			return len(accs), nil
		},
		Register: func(k autotask.Kind, count int) ([]string, error) {
			f.regCall = append(f.regCall, count)
			return []string{"robot0009001@xy3.com"}, nil
		},
		Launch: func(k autotask.Kind, accs []string) bool {
			f.launchCall = append(f.launchCall, accs)
			return true
		},
	}
}

func cfg() autotask.Config {
	return autotask.Config{IntervalSec: 300, JitterSec: 0, BatchMin: 1, BatchMax: 3, MaxOnline: 0}
}

// 在线空闲的号：直接下发，不需要上线、不需要等。
func TestLaunchOnlineCandidateImmediately(t *testing.T) {
	f := newFake()
	f.randVals = []int{1} // batch = 1 + rand(3) = 2
	f.candidates[autotask.KindGhost] = []autotask.Candidate{
		{Account: "b@x.com", Online: true}, {Account: "a@x.com", Online: true}, {Account: "c@x.com", Online: true},
	}
	r := autotask.New(f.deps())
	if err := r.Start(autotask.KindGhost, cfg()); err != nil {
		t.Fatal(err)
	}
	rounds := r.Tick(f.now)
	if len(rounds) != 1 || len(rounds[0].Picked) != 2 {
		t.Fatalf("每轮应按 BatchMin..BatchMax 随机挑 2 个: %+v", rounds)
	}
	if len(f.onlineCall) != 0 {
		t.Fatalf("都在线就不该再调上线: %v", f.onlineCall)
	}
	if len(f.launchCall) != 1 || len(f.launchCall[0]) != 2 {
		t.Fatalf("应直接下发两个号: %v", f.launchCall)
	}
	if st := r.States()[autotask.KindGhost]; st.Picked != 2 || !st.Enabled {
		t.Fatalf("状态应累计已拉起 2 个: %+v", st)
	}
	// 下一轮要等到 300 秒后（没到点不发）
	if rounds := r.Tick(f.now.Add(299 * time.Second)); len(rounds) != 0 {
		t.Fatalf("间隔没到不该再跑: %+v", rounds)
	}
	if rounds := r.Tick(f.now.Add(301 * time.Second)); len(rounds) != 1 {
		t.Fatalf("间隔到点应再跑一轮: %+v", rounds)
	}
}

// 离线的号：先上线，**等 8 秒**（等角色数据就绪）再下发任务。
func TestOfflineCandidateOnlineThenDelayedLaunch(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindNewbie] = []autotask.Candidate{{Account: "n1@x.com", Online: false}}
	r := autotask.New(f.deps())
	_ = r.Start(autotask.KindNewbie, autotask.Config{IntervalSec: 300, BatchMin: 1, BatchMax: 1})

	rounds := r.Tick(f.now)
	if len(rounds) != 1 || len(rounds[0].Online) != 1 {
		t.Fatalf("离线的号应走「上线」分支: %+v", rounds)
	}
	if len(f.onlineCall) != 1 || len(f.launchCall) != 0 {
		t.Fatalf("此刻只该上线、不该下发: online=%v launch=%v", f.onlineCall, f.launchCall)
	}
	if rounds := r.Tick(f.now.Add(7 * time.Second)); len(rounds) != 0 {
		t.Fatalf("8 秒没到不该下发: %+v", rounds)
	}
	rounds = r.Tick(f.now.Add(9 * time.Second))
	if len(rounds) != 1 || len(f.launchCall) != 1 {
		t.Fatalf("8 秒后应补发任务: %+v launch=%v", rounds, f.launchCall)
	}
}

// 同时在线上限：到顶就等空槽（15 秒后再看），不硬拉。
func TestMaxOnlineBlocks(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindGhost] = []autotask.Candidate{{Account: "g@x.com", Online: false}}
	f.onlineN[autotask.KindGhost] = 5
	r := autotask.New(f.deps())
	_ = r.Start(autotask.KindGhost, autotask.Config{IntervalSec: 300, BatchMin: 1, BatchMax: 3, MaxOnline: 5})

	if rounds := r.Tick(f.now); len(rounds) != 0 {
		t.Fatalf("达到上限不该再拉: %+v", rounds)
	}
	if len(f.onlineCall) != 0 || len(f.launchCall) != 0 {
		t.Fatal("达到上限不该有任何动作")
	}
	st := r.States()[autotask.KindGhost]
	if st.NextAt.Sub(f.now) != 15*time.Second {
		t.Fatalf("上限时应 15 秒后再看: %+v", st.NextAt)
	}
	// 槽位空出来 → 正常拉起
	f.onlineN[autotask.KindGhost] = 0
	f.now = f.now.Add(16 * time.Second)
	if rounds := r.Tick(f.now); len(rounds) != 1 {
		t.Fatalf("空出槽位后应恢复: %+v", rounds)
	}
}

// 没号可挑且开了自动注册 → 注册（按配置个数），并记进轮次。
func TestRegisterWhenNoCandidates(t *testing.T) {
	f := newFake()
	r := autotask.New(f.deps())
	_ = r.Start(autotask.KindNewbie, autotask.Config{IntervalSec: 300, RegisterEnabled: true, RegisterCount: 10})

	rounds := r.Tick(f.now)
	if len(rounds) != 1 || len(rounds[0].Registered) != 1 {
		t.Fatalf("没号时应发起注册并记录: %+v", rounds)
	}
	if len(f.regCall) != 1 || f.regCall[0] != 10 {
		t.Fatalf("应按配置注册 10 个: %v", f.regCall)
	}
	if st := r.States()[autotask.KindNewbie]; st.Registered != 1 {
		t.Fatalf("状态应累计注册数: %+v", st)
	}
	// 没开自动注册 → 什么都不做
	f2 := newFake()
	r2 := autotask.New(f2.deps())
	_ = r2.Start(autotask.KindNewbie, autotask.Config{IntervalSec: 300})
	if rounds := r2.Tick(f2.now); len(rounds) != 0 || len(f2.regCall) != 0 {
		t.Fatalf("没开自动注册不该注册: rounds=%v calls=%v", rounds, f2.regCall)
	}
}

// 停止后：不再拉起，且**撤掉延迟队列**（避免"我停了它又被拉起来"）。
func TestStopCancelsPendingLaunch(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindGhost] = []autotask.Candidate{{Account: "g@x.com", Online: false}}
	r := autotask.New(f.deps())
	_ = r.Start(autotask.KindGhost, cfg())
	r.Tick(f.now) // 上线 + 排队下发

	r.Stop(autotask.KindGhost)
	if rounds := r.Tick(f.now.Add(30 * time.Second)); len(rounds) != 0 {
		t.Fatalf("停止后不该再下发: %+v", rounds)
	}
	if len(f.launchCall) != 0 {
		t.Fatalf("停止后队列应被撤销: %v", f.launchCall)
	}
	if st := r.States()[autotask.KindGhost]; st.Enabled {
		t.Fatalf("状态应显示已停止: %+v", st)
	}
}

// 两个策略互不影响：停一个，另一个照跑。
func TestKindsAreIndependent(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindNewbie] = []autotask.Candidate{{Account: "n@x.com", Online: true}}
	f.candidates[autotask.KindGhost] = []autotask.Candidate{{Account: "g@x.com", Online: true}}
	r := autotask.New(f.deps())
	_ = r.Start(autotask.KindNewbie, autotask.Config{IntervalSec: 300, BatchMin: 1, BatchMax: 1})
	_ = r.Start(autotask.KindGhost, autotask.Config{IntervalSec: 300, BatchMin: 1, BatchMax: 1})

	r.Stop(autotask.KindNewbie)
	rounds := r.Tick(f.now)
	if len(rounds) != 1 || len(f.launchCall) != 1 || f.launchCall[0][0] != "g@x.com" {
		t.Fatalf("只该抓鬼这一轮跑: rounds=%+v launch=%v", rounds, f.launchCall)
	}
}

// 间隔抖动：实际间隔 = 基础 + rand(Jitter)（"5 分钟之内随机"= 基础 0 + 抖动 300）。
func TestIntervalJitter(t *testing.T) {
	f := newFake()
	f.randVals = []int{120} // BatchMin==BatchMax 时不摇批量，第一个随机数用在抖动上
	f.candidates[autotask.KindGhost] = []autotask.Candidate{{Account: "g@x.com", Online: true}}
	r := autotask.New(f.deps())
	_ = r.Start(autotask.KindGhost, autotask.Config{IntervalSec: 300, JitterSec: 300, BatchMin: 1, BatchMax: 1})

	r.Tick(f.now)
	st := r.States()[autotask.KindGhost]
	if got := st.NextAt.Sub(f.now); got != 420*time.Second {
		t.Fatalf("下一次应在 300+120 秒后（基础 300 + rand(301)=120）: %v", got)
	}
}

// 保持数（按目标补差额）：在跑 47 / 目标 50 → 本轮最多补 3 个；达标后不再拉。
func TestTargetOnlineRefillsOnlyDeficit(t *testing.T) {
	f := newFake()
	f.onlineN[autotask.KindGhost] = 47
	cands := []autotask.Candidate{}
	for _, a := range []string{"g1", "g2", "g3", "g4", "g5"} {
		cands = append(cands, autotask.Candidate{Account: a, Online: true})
	}
	f.candidates[autotask.KindGhost] = cands
	r := autotask.New(f.deps())
	_ = r.Start(autotask.KindGhost, autotask.Config{
		IntervalSec: 300, BatchMin: 5, BatchMax: 5, TargetOnline: 50, // 每轮最多 5，但只差 3
	})

	rounds := r.Tick(f.now)
	if len(rounds) != 1 || len(rounds[0].Picked) != 3 {
		t.Fatalf("只该补差额 3 个（在跑 47 / 目标 50）: %+v", rounds)
	}
	st := r.States()[autotask.KindGhost]
	if st.Target != 50 || st.Online != 47 || st.Deficit != 3 {
		t.Fatalf("状态应回带 在跑/目标/缺口: %+v", st)
	}

	// 达标后：本轮不再拉（只更新说明）
	f.onlineN[autotask.KindGhost] = 50
	f.now = f.now.Add(301 * time.Second)
	before := len(f.launchCall)
	if rounds := r.Tick(f.now); len(rounds) != 0 {
		t.Fatalf("达标后不该再拉起: %+v", rounds)
	}
	if len(f.launchCall) != before {
		t.Fatal("达标后不该有任何下发")
	}
	if msg := r.States()[autotask.KindGhost].LastMsg; msg == "" || !contains(msg, "已达标") {
		t.Fatalf("应说明已达标: %q", msg)
	}
}

// 回调契约（2026-09-24 生产死锁事故的配套）：Candidates 在 Runner **持锁时**被调用，
// 因此本策略的当前配置必须**随回调传入**（shenbu 的 min_level/balance_gate 判据靠它），
// 实现里不得再回读 Runner 方法（States() 等）——那会变成同 goroutine 锁重入 → 永久死锁。
func TestCandidatesReceiveLiveConfig(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindShenbu] = []autotask.Candidate{{Account: "s1@x.com", Online: true}}
	r := autotask.New(f.deps())
	if err := r.Start(autotask.KindShenbu, autotask.Config{
		TargetOnline: 5, BatchMin: 1, BatchMax: 1, MinLevel: 40, BalanceGate: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if rounds := r.Tick(f.now); len(rounds) != 1 {
		t.Fatalf("shenbu 应挑中在线候选并下发: %+v", rounds)
	}
	got, ok := f.candCfg[autotask.KindShenbu]
	if !ok {
		t.Fatal("Candidates 回调未被调用（挑号路径没走到）")
	}
	if got.MinLevel != 40 || got.BalanceGate != 1000 || got.TargetOnline != 5 {
		t.Fatalf("Candidates 必须收到本策略当前配置（min_level/balance_gate 靠它判定）: %+v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
