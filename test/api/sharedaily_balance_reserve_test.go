// ⑤ 余额闸改储备金口径（2026-09-29 神捕闸门修复⑤）：
// 旧实现只看银两（r.Money）→ 现场在线 230 里 227 个 0<money<1000 被挡死（储备金却常几十万级）。
// 新口径：以储备金为准，储备金缺失（0）回退银两；两者都"未知"（0）不拦。
package api_test

import (
	"testing"

	"zyctrlcenter/internal/services/autotask"
)

// startShenbuWithGate 起神捕池并显式开余额闸（BalanceGate 默认 0=不启用；不 Start 则测不到闸）。
func startShenbuWithGate(t *testing.T, env *testEnv, gate int) {
	t.Helper()
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 600, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
		MinLevel: 40, BalanceGate: gate,
	}); err != nil {
		t.Fatalf("起神捕池失败: %v", err)
	}
}

func candidateCountShenbu(t *testing.T, env *testEnv) float64 {
	t.Helper()
	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	n, _ := cands["shenbu"].(float64)
	return n
}

func TestShareDailyBalanceGateUsesReserve(t *testing.T) {
	// ① 储备足（26 万）+ 银两小额（36，旧口径会被挡）→ 应进候选
	env := newTestEnv(t, "")
	startShenbuWithGate(t, env, 1000)
	addUsableAccount(t, env, "bal_ok@xy3.com", 45)
	feedRobot(t, env, "bal_ok@xy3.com", map[string]any{"money": 36, "reserve": 260561,
		"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 5, "limit": 10, "state": "READY"}})
	if n := candidateCountShenbu(t, env); n != 1 {
		t.Fatalf("储备足应放行（旧口径按银两会误挡），实际 %v", n)
	}

	// ② 储备不足（500 < gate 1000）→ 拦
	env2 := newTestEnv(t, "")
	startShenbuWithGate(t, env2, 1000)
	addUsableAccount(t, env2, "bal_low@xy3.com", 45)
	feedRobot(t, env2, "bal_low@xy3.com", map[string]any{"money": 50000, "reserve": 500,
		"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 5, "limit": 10, "state": "READY"}})
	if n := candidateCountShenbu(t, env2); n != 0 {
		t.Fatalf("储备不足应拦下（银两再多也不够采购），实际 %v", n)
	}

	// ③ 储备缺失（0）→ 回退银两：银两 200 ∈ (0,1000) → 拦
	env3 := newTestEnv(t, "")
	startShenbuWithGate(t, env3, 1000)
	addUsableAccount(t, env3, "bal_fb@xy3.com", 45)
	feedRobot(t, env3, "bal_fb@xy3.com", map[string]any{"money": 200,
		"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 5, "limit": 10, "state": "READY"}})
	if n := candidateCountShenbu(t, env3); n != 0 {
		t.Fatalf("储备缺失应回退银两口径（200<1000 → 拦），实际 %v", n)
	}

	// ④ 两者都未知（0）→ 不拦（旧口径保持）
	env4 := newTestEnv(t, "")
	startShenbuWithGate(t, env4, 1000)
	addUsableAccount(t, env4, "bal_unk@xy3.com", 45)
	feedRobot(t, env4, "bal_unk@xy3.com", map[string]any{
		"daily": map[string]any{"share_key": "share_daily_大唐神捕", "done": 5, "limit": 10, "state": "READY"}})
	if n := candidateCountShenbu(t, env4); n != 1 {
		t.Fatalf("资金未知不该拦，实际 %v", n)
	}
}
