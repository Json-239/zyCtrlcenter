// 意图表 P0：判据（等级/链完成 → 哪条链）+ 一个账号同一时刻只有一条链 + 快照/计数。
//
// 阈值口径与参考实现一致：等级 < 31 且新手链未完成 → 新手链优先；≥31 或已完成 → 抓鬼。
package intent_test

import (
	"testing"

	"zyctrlcenter/internal/services/intent"
)

func TestDecideNewbieForLowLevel(t *testing.T) {
	d := intent.Decider{}
	dec := d.Decide(20, false)
	if !dec.Known || dec.Kind != intent.KindNewbie {
		t.Fatalf("20 级未跑完新手链应判新手链: %+v", dec)
	}
	if dec.ChainID != "newbie_full" {
		t.Fatalf("应带默认链 id: %+v", dec)
	}
	if dec.Reason == "" {
		t.Fatal("要给出判据理由（面板/排障要看）")
	}
}

func TestDecideGhostAtThreshold(t *testing.T) {
	d := intent.Decider{}
	for _, lv := range []int{31, 32, 45, 90} {
		dec := d.Decide(lv, false)
		if dec.Kind != intent.KindGhost {
			t.Fatalf("%d 级（≥31）应判抓鬼: %+v", lv, dec)
		}
	}
	// 边界：30 仍是新手链
	if dec := d.Decide(30, false); dec.Kind != intent.KindNewbie {
		t.Fatalf("30 级应仍是新手链: %+v", dec)
	}
}

func TestDecideGhostWhenChainDone(t *testing.T) {
	d := intent.Decider{}
	dec := d.Decide(20, true) // 等级低但新手链已完成
	if dec.Kind != intent.KindGhost {
		t.Fatalf("新手链已完成应转抓鬼（别重复跑）: %+v", dec)
	}
}

func TestDecideUnknownLevelIsPending(t *testing.T) {
	d := intent.Decider{}
	for _, lv := range []int{0, -1} {
		dec := d.Decide(lv, false)
		if dec.Known || dec.Kind != "" {
			t.Fatalf("等级未知时不能瞎判（等上线报一次等级）: lv=%d %+v", lv, dec)
		}
	}
}

func TestDeciderThresholdIsConfigurable(t *testing.T) {
	d := intent.Decider{NewbieMaxLevel: 45, NewbieChainID: "newbie_x"}
	if dec := d.Decide(40, false); dec.Kind != intent.KindNewbie || dec.ChainID != "newbie_x" {
		t.Fatalf("阈值/链 id 应可配: %+v", dec)
	}
	if dec := d.Decide(45, false); dec.Kind != intent.KindGhost {
		t.Fatalf("自定义阈值 45 时 45 级应为抓鬼: %+v", dec)
	}
}

// 一个账号同一时刻只能有一条链：切换时返回上一个意图（调用方据此先停旧链）。
func TestPlanSingleKindPerAccount(t *testing.T) {
	d := intent.Decider{}
	p := intent.NewPlan(d)

	prev, changed, err := p.Apply("a1@x.com", d.Decide(20, false), "47.96.8.240:2300", "decide")
	if err != nil || !changed || prev.Kind != "" {
		t.Fatalf("首次登记应 changed 且 prev 为空: prev=%+v changed=%v err=%v", prev, changed, err)
	}
	if it, ok := p.Get("a1@x.com"); !ok || it.Kind != intent.KindNewbie {
		t.Fatalf("应登记新手链意图: %+v", it)
	}

	// 升到 45 级 → 转抓鬼：prev 必须是新手链（好让调用方停旧链）
	prev, changed, err = p.Apply("a1@x.com", d.Decide(45, false), "47.96.8.240:2300", "decide")
	if err != nil || !changed {
		t.Fatalf("切换应 changed: %v %v", changed, err)
	}
	if prev.Kind != intent.KindNewbie {
		t.Fatalf("prev 应是切换前的意图（新手链）: %+v", prev)
	}
	it, _ := p.Get("a1@x.com")
	if it.Kind != intent.KindGhost {
		t.Fatalf("切换后应是抓鬼: %+v", it)
	}
	if p.Conflict("a1@x.com", intent.KindNewbie) != true {
		t.Fatal("正跑抓鬼时，再想跑新手链应算冲突（上线前校验）")
	}
	if p.Conflict("a1@x.com", intent.KindGhost) {
		t.Fatal("跑的就是抓鬼：同一条链不算冲突（幂等重发）")
	}

	// 同样的判定重复登记：不算变化（幂等，避免把 Since 刷成新时间）
	prev, changed, err = p.Apply("a1@x.com", d.Decide(45, false), "47.96.8.240:2300", "decide")
	if err != nil || changed || prev.Kind != intent.KindGhost {
		t.Fatalf("重复登记应幂等: prev=%+v changed=%v err=%v", prev, changed, err)
	}
}

func TestPlanRejectsEmptyAccountAndUnknownLevel(t *testing.T) {
	d := intent.Decider{}
	p := intent.NewPlan(d)
	if _, _, err := p.Apply("  ", d.Decide(20, false), "", "decide"); err == nil {
		t.Fatal("账号为空应报错")
	}
	// 等级未知：不登记，也不覆盖已有意图
	_, _, _ = p.Apply("a2@x.com", d.Decide(20, false), "", "decide")
	if _, _, err := p.Apply("a2@x.com", d.Decide(0, false), "", "decide"); err == nil {
		t.Fatal("等级未知应明确报错（调用方不要再瞎判）")
	}
	if it, _ := p.Get("a2@x.com"); it.Kind != intent.KindNewbie {
		t.Fatalf("未知等级不应覆盖已登记意图: %+v", it)
	}
}

func TestPlanSnapshotCountsAndRemove(t *testing.T) {
	d := intent.Decider{}
	p := intent.NewPlan(d)
	_, _, _ = p.Apply("b@x.com", d.Decide(45, false), "z1", "decide")
	_, _, _ = p.Apply("a@x.com", d.Decide(20, false), "z1", "decide")
	_, _, _ = p.Apply("c@x.com", d.Decide(20, true), "z1", "decide")

	snap := p.Snapshot()
	if len(snap) != 3 || snap[0].Account != "a@x.com" || snap[2].Account != "c@x.com" {
		t.Fatalf("快照应按账号排序: %+v", snap)
	}
	counts := p.Counts()
	if counts[string(intent.KindNewbie)] != 1 || counts[string(intent.KindGhost)] != 2 {
		t.Fatalf("计数不符（新手 1 / 抓鬼 2）: %v", counts)
	}
	if !p.Remove("a@x.com") || p.Remove("a@x.com") {
		t.Fatal("删除应返回是否真的删到了")
	}
	if _, ok := p.Get("a@x.com"); ok {
		t.Fatal("删除后不该还在")
	}
}
