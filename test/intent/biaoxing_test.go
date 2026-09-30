// 镖行天下判据（2026-09-30 首期 40-59 档接入，分享日常家族第三成员）：
//
//	等级 ≥40 + 心跳明确"今日未满" + BiaoxingEnabled（**默认关**）→ 镖行天下；
//	关/未知/满 → 回落旧判据（抓鬼/新手链）；三玩法都就绪时按 shenbu → fenghuo → biaoxing。
//
// 关键防回归点：默认关时绝不判 biaoxing（首期只手动试点；否则全量 ≥40 号被判去押镖）。
package intent_test

import (
	"testing"

	"zyctrlcenter/internal/services/intent"
)

func TestDecideDailyBiaoxingWhenReady(t *testing.T) {
	d := intent.Decider{BiaoxingEnabled: true}
	days := intent.DailyStates{Biaoxing: intent.DailyInfo{Known: true}}
	dec := d.DecideDailyStates(45, false, days)
	if !dec.Known || dec.Kind != intent.KindBiaoxing {
		t.Fatalf("45 级 + 镖行天下今日未满 + 开关启用 应判镖行天下: %+v", dec)
	}
	if dec.Reason == "" {
		t.Fatal("要给出判据理由")
	}
	// 已毕业同样可判
	if dec := d.DecideDailyStates(45, true, days); dec.Kind != intent.KindBiaoxing {
		t.Fatalf("已毕业的 45 级号也应判镖行天下: %+v", dec)
	}
}

func TestDecideDailyBiaoxingFallbacks(t *testing.T) {
	on := intent.Decider{BiaoxingEnabled: true, BiaoxingMinLevel: 40}
	off := intent.Decider{} // 默认（关）
	cases := []struct {
		name string
		d    intent.Decider
		lv   int
		info intent.DailyInfo
	}{
		{"心跳未知（老版机器人）", on, 45, intent.DailyInfo{}},
		{"今日已满", on, 45, intent.DailyInfo{Known: true, Full: true}},
		{"开关未启用（默认生产口径）", off, 45, intent.DailyInfo{Known: true}},
		{"39 级未达门槛（31-39 → 抓鬼）", on, 39, intent.DailyInfo{Known: true}},
	}
	for _, c := range cases {
		if dec := c.d.DecideDailyStates(c.lv, false, intent.DailyStates{Biaoxing: c.info}); dec.Kind != intent.KindGhost {
			t.Fatalf("%s：应回落抓鬼: %+v", c.name, dec)
		}
	}
}

// 三玩法都就绪 → shenbu 优先（队列固定次序 ghost→newbie→shenbu→fenghuo→biaoxing 同口径）。
func TestDecideDailyBiaoxingPriority(t *testing.T) {
	d := intent.Decider{ShareDailyEnabled: true, FenghuoEnabled: true, BiaoxingEnabled: true}
	known := intent.DailyInfo{Known: true}
	days := intent.DailyStates{Shenbu: known, Fenghuo: known, Biaoxing: known}
	if dec := d.DecideDailyStates(45, false, days); dec.Kind != intent.KindShenbu {
		t.Fatalf("三玩法都就绪应 shenbu 优先: %+v", dec)
	}
	// 神捕满、烽火未满 → 烽火优先于镖行
	days2 := intent.DailyStates{Shenbu: intent.DailyInfo{Known: true, Full: true}, Fenghuo: known, Biaoxing: known}
	if dec := d.DecideDailyStates(45, false, days2); dec.Kind != intent.KindFenghuo {
		t.Fatalf("神捕满时应先烽火: %+v", dec)
	}
	// 神捕/烽火都满 → 镖行
	days3 := intent.DailyStates{
		Shenbu: intent.DailyInfo{Known: true, Full: true}, Fenghuo: intent.DailyInfo{Known: true, Full: true}, Biaoxing: known}
	if dec := d.DecideDailyStates(45, false, days3); dec.Kind != intent.KindBiaoxing {
		t.Fatalf("前两玩法都满时应判镖行天下: %+v", dec)
	}
}
