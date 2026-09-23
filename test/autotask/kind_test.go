// 策略 Kind 枚举：shenbu（大唐神捕）的 Valid/Label/Kinds 覆盖（清单 G1 验收点：
// Valid() 覆盖新 Kind；未知 Kind 拒绝）。
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

func TestStartRejectsUnknownKind(t *testing.T) {
	r := autotask.New(autotask.Deps{})
	if err := r.Start(autotask.Kind("fenghuo"), autotask.Config{}); err == nil {
		t.Fatal("未实现的玩法（烽火大唐 P1）当前必须被拒绝")
	}
	if err := r.Start(autotask.KindShenbu, autotask.Config{}); err != nil {
		t.Fatalf("shenbu 应可启动: %v", err)
	}
}
