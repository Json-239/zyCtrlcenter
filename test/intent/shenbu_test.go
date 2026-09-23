// 大唐神捕（分享日常）判据（2026-09-23 接入，方案 §4.3 / 清单 G2）：
//
//	等级 ≥40 + 心跳明确"今日未满" + 开关启用 → 大唐神捕（shenbu）；
//	否则**回落旧判据**：31-39 → 抓鬼、<31（未毕业）→ 新手链、等级未知 → 待定。
//
// 关键防回归点：开关默认关、心跳缺失（老版机器人）= 未知 → 绝不判 shenbu
// （否则生产上所有 ≥40 号会被判去神捕，抓鬼池瞬间空掉）。
package intent_test

import (
	"testing"

	"zyctrlcenter/internal/services/intent"
)

func TestDecideDailyShenbuWhenReady(t *testing.T) {
	d := intent.Decider{ShareDailyEnabled: true}
	dec := d.DecideDaily(45, false, intent.DailyInfo{Known: true})
	if !dec.Known || dec.Kind != intent.KindShenbu {
		t.Fatalf("45 级 + 神捕今日未满 + 开关启用 应判大唐神捕: %+v", dec)
	}
	if dec.Reason == "" {
		t.Fatal("要给出判据理由（面板/排障要看）")
	}
	// 新手链已完成（chainDone）的 ≥40 号同样可以判神捕
	if dec := d.DecideDaily(45, true, intent.DailyInfo{Known: true}); dec.Kind != intent.KindShenbu {
		t.Fatalf("已毕业的 45 级号也应判大唐神捕: %+v", dec)
	}
}

func TestDecideDailyFallsBackToGhost(t *testing.T) {
	on := intent.Decider{ShareDailyEnabled: true, ShareDailyMinLevel: 40}
	off := intent.Decider{ShareDailyMinLevel: 40} // 开关未启用（默认生产口径）
	cases := []struct {
		name string
		d    intent.Decider
		lv   int
		info intent.DailyInfo
	}{
		{"心跳未知（老版机器人不带 daily）", on, 45, intent.DailyInfo{}},
		{"今日已满", on, 45, intent.DailyInfo{Known: true, Full: true}},
		{"开关未启用", off, 45, intent.DailyInfo{Known: true}},
		{"39 级未达神捕门槛（31-39 → 抓鬼）", on, 39, intent.DailyInfo{Known: true}},
		{"31 级刚毕业（31-39 → 抓鬼）", on, 31, intent.DailyInfo{Known: true}},
	}
	for _, c := range cases {
		if dec := c.d.DecideDaily(c.lv, false, c.info); dec.Kind != intent.KindGhost {
			t.Fatalf("%s：应回落抓鬼: %+v", c.name, dec)
		}
	}
}

func TestDecideDailyNewbieStillWinsBelowThreshold(t *testing.T) {
	d := intent.Decider{ShareDailyEnabled: true}
	if dec := d.DecideDaily(25, false, intent.DailyInfo{Known: true}); dec.Kind != intent.KindNewbie {
		t.Fatalf("25 级未毕业仍应是新手链（<31 → newbie）: %+v", dec)
	}
}

func TestDecideOldEntryKeepsLegacyBehavior(t *testing.T) {
	d := intent.Decider{ShareDailyEnabled: true}
	if dec := d.Decide(45, false); dec.Kind != intent.KindGhost {
		t.Fatalf("旧入口 Decide（不带 daily 信息=未知）必须保持旧行为（≥31→抓鬼）: %+v", dec)
	}
}

func TestDecideDailyUnknownLevelPending(t *testing.T) {
	d := intent.Decider{ShareDailyEnabled: true}
	for _, lv := range []int{0, -1} {
		if dec := d.DecideDaily(lv, false, intent.DailyInfo{Known: true}); dec.Known {
			t.Fatalf("等级 %d 未知应待定（不瞎跑）: %+v", lv, dec)
		}
	}
}

func TestShareDailyKeyDefault(t *testing.T) {
	if got := (intent.Decider{}).ShareDailyKeyOf(); got != intent.DefaultShareDailyKey {
		t.Fatalf("默认玩法键应为 %s，实际 %s", intent.DefaultShareDailyKey, got)
	}
	if got := (intent.Decider{ShareDailyKey: "share_daily_宫廷10"}).ShareDailyKeyOf(); got != "share_daily_宫廷10" {
		t.Fatalf("显式配置的玩法键应原样返回，实际 %s", got)
	}
}
