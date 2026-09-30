// 策略 Kind 枚举：shenbu（大唐神捕）/ fenghuo（烽火大唐）的 Valid/Label/Kinds 覆盖
// （清单 G1 验收点：Valid() 覆盖新 Kind；未知 Kind 拒绝；Kinds 次序稳定）。
package autotask_test

import (
	"testing"

	"zyctrlcenter/internal/services/autotask"
)

func TestKindShenbuValidAndLabel(t *testing.T) {
	if !autotask.KindShenbu.Valid() {
		t.Fatal("shenbu 必须是受支持的策略")
	}
	if got := autotask.KindShenbu.Label(); got != "大唐神捕" {
		t.Fatalf("shenbu 的中文名应为「大唐神捕」，实际 %q", got)
	}
	if autotask.Kind("不存在").Valid() {
		t.Fatal("未知策略必须被拒绝")
	}
}

// 烽火大唐（2026-09-24 P1）：Kind 枚举进 Valid/Label；错误文案列全五个。
func TestKindFenghuoValidAndLabel(t *testing.T) {
	if !autotask.KindFenghuo.Valid() {
		t.Fatal("fenghuo 必须是受支持的策略")
	}
	if got := autotask.KindFenghuo.Label(); got != "烽火大唐" {
		t.Fatalf("fenghuo 的中文名应为「烽火大唐」，实际 %q", got)
	}
}

func TestKindsIncludeShenbu(t *testing.T) {
	// Kinds 是面板/落盘/保存恢复的驱动列表：新 kind 必须在里面（否则策略卡与持久化都拿不到）
	found := false
	for _, k := range autotask.Kinds {
		if k == autotask.KindShenbu {
			found = true
		}
	}
	if !found {
		t.Fatalf("Kinds 必须包含 shenbu: %v", autotask.Kinds)
	}
}

// Kinds 固定次序 newbie→ghost→hatch→shenbu→fenghuo→biaoxing：
// 前端「日常轮转」queue 的固定排序（ghost→newbie→shenbu→fenghuo→biaoxing）与它同口径，勿改。
// 注：biaoxing（镖行天下，2026-09-30 首期）默认关/target=0，位次先占好。
func TestKindsOrderPinsDailyFamily(t *testing.T) {
	want := []autotask.Kind{autotask.KindNewbie, autotask.KindGhost, autotask.KindHatch,
		autotask.KindShenbu, autotask.KindFenghuo, autotask.KindBiaoxing}
	if len(autotask.Kinds) != len(want) {
		t.Fatalf("Kinds 应恰有 %d 项（%v），实际 %v", len(want), want, autotask.Kinds)
	}
	for i := range want {
		if autotask.Kinds[i] != want[i] {
			t.Fatalf("Kinds[%d] 应为 %s，实际 %s（次序被前端队列依赖，勿改）",
				i, want[i], autotask.Kinds[i])
		}
	}
}

func TestStartRejectsUnknownKind(t *testing.T) {
	r := autotask.New(autotask.Deps{})
	if err := r.Start(autotask.Kind("fenghuo_p2"), autotask.Config{}); err == nil {
		t.Fatal("不存在的玩法必须被拒绝（错误文案应列出全部合法 kind）")
	}
	if err := r.Start(autotask.KindShenbu, autotask.Config{}); err != nil {
		t.Fatalf("shenbu 应可启动: %v", err)
	}
	// 2026-09-24：fenghuo 已接入（P1），必须可启动
	if err := r.Start(autotask.KindFenghuo, autotask.Config{}); err != nil {
		t.Fatalf("fenghuo 应可启动: %v", err)
	}
}
