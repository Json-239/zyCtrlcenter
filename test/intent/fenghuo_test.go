// 烽火大唐（fenghuo）判据（2026-09-24 P1 接入，与 shenbu 同款）：
//
//	等级 ≥40 + 该玩法心跳明确"今日未满" + 开关启用 → 烽火大唐；
//	两个玩法（shenbu/fenghuo）都满足时 **shenbu 优先**（与 Kinds/前端队列固定次序同口径）；
//	否则回落旧判据：31-39 → 抓鬼、<31（未毕业）→ 新手链、等级未知 → 待定。
//
// 关键防回归点：开关默认关、心跳缺失（老版机器人）= 未知 → 绝不判 fenghuo
// （否则升级中控就会把号从抓鬼池/神捕池抢走）。
package intent_test

import (
	"testing"

	"zyctrlcenter/internal/services/intent"
)

// 开关默认关（未配 FenghuoEnabled）+ 心跳明确未满 → **不判** fenghuo（保守，回落抓鬼）。
func TestDecideFenghuoDisabledStaysConservative(t *testing.T) {
	d := intent.Decider{} // FenghuoEnabled=false（生产默认）
	dec := d.DecideDailyStates(45, false,
		intent.DailyStates{Fenghuo: intent.DailyInfo{Known: true}})
	if dec.Kind != intent.KindGhost {
		t.Fatalf("开关默认关：烽火大唐未满也必须回落抓鬼（保守口径）: %+v", dec)
	}
}

// 开关开 + 45 级 + 烽火大唐未满 → 判 fenghuo。
func TestDecideFenghuoWhenReady(t *testing.T) {
	d := intent.Decider{FenghuoEnabled: true}
	dec := d.DecideDailyStates(45, false,
		intent.DailyStates{Fenghuo: intent.DailyInfo{Known: true}})
	if !dec.Known || dec.Kind != intent.KindFenghuo {
		t.Fatalf("45 级 + 烽火大唐今日未满 + 开关启用 应判烽火大唐: %+v", dec)
	}
	if dec.Reason == "" {
		t.Fatal("要给出判据理由（面板/排障要看）")
	}
}

// 开关开但心跳缺失（老版机器人不带 daily）= 未知 → 不判 fenghuo。
func TestDecideFenghuoUnknownHeartbeatFallsBack(t *testing.T) {
	d := intent.Decider{FenghuoEnabled: true}
	if dec := d.DecideDailyStates(45, false, intent.DailyStates{}); dec.Kind != intent.KindGhost {
		t.Fatalf("心跳未知（没有 daily 块）不得判烽火大唐: %+v", dec)
	}
}

// 开关开 + 今日已满 → 回落抓鬼（不占烽火大唐名额）。
func TestDecideFenghuoFullFallsBackToGhost(t *testing.T) {
	d := intent.Decider{FenghuoEnabled: true}
	dec := d.DecideDailyStates(45, false,
		intent.DailyStates{Fenghuo: intent.DailyInfo{Known: true, Full: true}})
	if dec.Kind != intent.KindGhost {
		t.Fatalf("烽火大唐已满应回落抓鬼: %+v", dec)
	}
}

// 门槛：39 级（未达 40）→ 抓鬼；25 级未毕业 → 新手链；等级未知 → 待定。
func TestDecideFenghuoLevelThresholds(t *testing.T) {
	d := intent.Decider{FenghuoEnabled: true}
	fh := intent.DailyStates{Fenghuo: intent.DailyInfo{Known: true}}
	if dec := d.DecideDailyStates(39, false, fh); dec.Kind != intent.KindGhost {
		t.Fatalf("39 级未达烽火大唐门槛（40）应回落抓鬼: %+v", dec)
	}
	if dec := d.DecideDailyStates(25, false, fh); dec.Kind != intent.KindNewbie {
		t.Fatalf("25 级未毕业仍应是新手链: %+v", dec)
	}
	if dec := d.DecideDailyStates(0, false, fh); dec.Known {
		t.Fatalf("等级未知应待定（不瞎跑）: %+v", dec)
	}
}

// 两个玩法都满足（开关都开、都未满）→ **shenbu 优先**（Kinds 次序/前端队列同口径）。
func TestDecideBothDailiesShenbuWins(t *testing.T) {
	d := intent.Decider{ShareDailyEnabled: true, FenghuoEnabled: true}
	both := intent.DailyStates{
		Shenbu:  intent.DailyInfo{Known: true},
		Fenghuo: intent.DailyInfo{Known: true},
	}
	if dec := d.DecideDailyStates(45, false, both); dec.Kind != intent.KindShenbu {
		t.Fatalf("两个玩法都满足时应 shenbu 优先（先跑满一条再转下一条）: %+v", dec)
	}
}

// 神捕满额 + 烽火大唐未满（都开）→ 转 fenghuo（"跑满一条转下一条"）。
func TestDecideShenbuFullThenFenghuo(t *testing.T) {
	d := intent.Decider{ShareDailyEnabled: true, FenghuoEnabled: true}
	days := intent.DailyStates{
		Shenbu:  intent.DailyInfo{Known: true, Full: true},
		Fenghuo: intent.DailyInfo{Known: true},
	}
	if dec := d.DecideDailyStates(45, false, days); dec.Kind != intent.KindFenghuo {
		t.Fatalf("神捕满额后应转烽火大唐: %+v", dec)
	}
}

// 神捕开关关 + 烽火大唐开 → 只判 fenghuo（两个开关互不影响）。
func TestDecideFenghuoIndependentOfShenbuFlag(t *testing.T) {
	d := intent.Decider{FenghuoEnabled: true} // ShareDailyEnabled=false
	days := intent.DailyStates{
		Shenbu:  intent.DailyInfo{Known: true}, // 神捕信息在也不判（开关关）
		Fenghuo: intent.DailyInfo{Known: true},
	}
	if dec := d.DecideDailyStates(45, false, days); dec.Kind != intent.KindFenghuo {
		t.Fatalf("神捕开关关时不应影响烽火大唐判定: %+v", dec)
	}
}

// 玩法键：默认 share_daily_宫廷10；显式配置原样返回。
func TestFenghuoKeyDefault(t *testing.T) {
	if got := (intent.Decider{}).FenghuoKeyOf(); got != intent.DefaultFenghuoKey {
		t.Fatalf("默认玩法键应为 %s，实际 %s", intent.DefaultFenghuoKey, got)
	}
	if got := (intent.Decider{FenghuoKey: "share_daily_别的"}).FenghuoKeyOf(); got != "share_daily_别的" {
		t.Fatalf("显式配置的玩法键应原样返回，实际 %s", got)
	}
	if intent.DefaultFenghuoKey != "share_daily_宫廷10" {
		t.Fatalf("默认玩法键必须与 20021.xml 的 share_daily_key 一致，实际 %s", intent.DefaultFenghuoKey)
	}
}

// 兼容入口：DecideDaily（只带 shenbu 信息）不受 fenghuo 字段影响（旧调用/旧测试路径）。
func TestDecideDailyCompatEntryIgnoresFenghuo(t *testing.T) {
	d := intent.Decider{FenghuoEnabled: true, ShareDailyEnabled: true}
	if dec := d.DecideDaily(45, false, intent.DailyInfo{}); dec.Kind != intent.KindGhost {
		t.Fatalf("旧入口不带 fenghuo 信息：不得判烽火大唐（保守回落抓鬼）: %+v", dec)
	}
}
