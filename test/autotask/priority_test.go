// 2026-09-23 有领双的必须优先抓鬼（用户口径）：Priority 候选**先挑**。
//
// 背景：领双（双倍经验）有时长，领了要尽快消耗掉；壳层（api/autotask.go）把"今日已领
// 双倍"的抓鬼候选标 Priority，引擎必须保证它不被随机洗牌挤掉。
package autotask_test

import (
	"strings"
	"testing"

	"zyctrlcenter/internal/services/autotask"
)

// n=1：Priority 候选在列表**末尾**也应被挑中（普通候选一个不挑）。
func TestPriorityCandidatePickedFirst(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindGhost] = []autotask.Candidate{
		{Account: "n1@x.com", Online: true},
		{Account: "n2@x.com", Online: true},
		{Account: "p9@x.com", Online: true, Priority: true, Reason: "抓鬼（45 级·今日领双优先）"},
	}
	r := autotask.New(f.deps())
	if err := r.Start(autotask.KindGhost, autotask.Config{IntervalSec: 300, BatchMin: 1, BatchMax: 1}); err != nil {
		t.Fatal(err)
	}
	rounds := r.Tick(f.now)
	if len(rounds) != 1 || strings.Join(rounds[0].Picked, ",") != "p9@x.com" {
		t.Fatalf("应优先挑领双号 p9@x.com，实际 %+v", rounds)
	}
	if len(f.launchCall) != 1 || strings.Join(f.launchCall[0], ",") != "p9@x.com" {
		t.Fatalf("下发的应只有领双号，实际 %v", f.launchCall)
	}
	if !strings.Contains(rounds[0].Msg, "领双") {
		t.Fatalf("轮次文案应能看出「含今日领双优先」，实际 %q", rounds[0].Msg)
	}
}

// n=2：1 个 Priority + 1 个普通 = 优先的先进，不够才用普通候选补足。
func TestPriorityThenFillWithNormal(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindGhost] = []autotask.Candidate{
		{Account: "n1@x.com", Online: true},
		{Account: "n2@x.com", Online: true},
		{Account: "p9@x.com", Online: true, Priority: true},
	}
	r := autotask.New(f.deps())
	if err := r.Start(autotask.KindGhost, autotask.Config{IntervalSec: 300, BatchMin: 2, BatchMax: 2}); err != nil {
		t.Fatal(err)
	}
	rounds := r.Tick(f.now)
	if len(rounds) != 1 || strings.Join(rounds[0].Picked, ",") != "n1@x.com,p9@x.com" {
		t.Fatalf("应先挑 p9、再补 1 个普通号，实际 %+v", rounds)
	}
}

// 没有 Priority（旧行为）→ 仍按随机批量挑，不受影响。
func TestNoPriorityKeepsOldBehavior(t *testing.T) {
	f := newFake()
	f.candidates[autotask.KindGhost] = []autotask.Candidate{
		{Account: "n1@x.com", Online: true},
		{Account: "n2@x.com", Online: true},
	}
	r := autotask.New(f.deps())
	if err := r.Start(autotask.KindGhost, autotask.Config{IntervalSec: 300, BatchMin: 1, BatchMax: 1}); err != nil {
		t.Fatal(err)
	}
	rounds := r.Tick(f.now)
	if len(rounds) != 1 || len(rounds[0].Picked) != 1 {
		t.Fatalf("无 Priority 时按随机批量挑 1 个，实际 %+v", rounds)
	}
	if strings.Contains(rounds[0].Msg, "领双") {
		t.Fatalf("无领双号时文案不该提领双，实际 %q", rounds[0].Msg)
	}
}
