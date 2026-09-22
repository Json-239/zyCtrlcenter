// 注册自适应限速（api.CreateThrottle）用例：
//   - 状态机：连续 112 → 并发减半 / 批间隔加倍（到顶 60s）；连续成功 CreateRecoverAfter 次 → 逐步回升；
//   - 节奏：批间隔带 ±jitter 抖动；请求只能更保守（不得比限速器当前值更快）；
//   - 配置默认值（CTRL_CREATE_*：自适应开、并发 8/≥2、批间隔 5s、抖动 ±2s）；
//   - HTTP 侧：建号响应回带 throttle 统计，112 真的把有效并发打下来。
//
// 口径：**不动协议字段**（106/104/700 与 8 个字段不变），只调"节奏"。
package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/api"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/test/testsupport"
)

// rateCfg 与线上默认一致的限速参数（8 并发 / ≥2 / 5s 基准 / ±2s 抖动）。
func rateCfg() api.CreateRateConfig {
	return api.CreateRateConfig{Adaptive: true, MaxConcurrency: 8, MinConcurrency: 2, BaseIntervalSec: 5, JitterSec: 2}
}

// 默认配置：自适应开、8/2、5s、±2s（CTRL_CREATE_* 的默认值）。
func TestCreateThrottleConfigDefaults(t *testing.T) {
	cfg := config.Default()
	if !cfg.CreateAdaptive {
		t.Fatal("CTRL_CREATE_ADAPTIVE 默认应为开（保守：撞 112 自动降速）")
	}
	if cfg.CreateMaxConcurrency != 8 {
		t.Fatalf("CTRL_CREATE_MAX_CONCURRENCY 默认应为 8，实际 %d", cfg.CreateMaxConcurrency)
	}
	if cfg.CreateMinConcurrency != 2 {
		t.Fatalf("CTRL_CREATE_MIN_CONCURRENCY 默认应为 2（降速下限），实际 %d", cfg.CreateMinConcurrency)
	}
	if cfg.CreateBatchIntervalSec != 5 {
		t.Fatalf("CTRL_CREATE_BATCH_INTERVAL_SEC 默认应为 5，实际 %d", cfg.CreateBatchIntervalSec)
	}
	if cfg.CreateJitterSec != 2 {
		t.Fatalf("CTRL_CREATE_JITTER_SEC 默认应为 2，实际 %d", cfg.CreateJitterSec)
	}
}

// 连续 112：并发 8→4→2（下限），批间隔 5→10→20→40→60（上限）。
func TestCreateThrottleSlowsOn112AndCapsInterval(t *testing.T) {
	lim := api.NewCreateThrottle(rateCfg())
	if st := lim.Stats(); st.Concurrency != 8 || st.BatchIntervalSec != 5 || st.TotalOK != 0 {
		t.Fatalf("初始节奏应为 8 并发 / 5s，实际 %+v", st)
	}
	lim.Observe(gameproto.RegErrServerFail)
	st := lim.Stats()
	if st.Concurrency != 4 || st.BatchIntervalSec != 10 {
		t.Fatalf("第 1 次 112 应减半到 4 / 10s，实际 %+v", st)
	}
	if st.Streak112 != 1 || st.Total112 != 1 {
		t.Fatalf("应记连续/累计 112 各 1，实际 %+v", st)
	}
	lim.Observe(gameproto.RegErrServerFail)
	if st = lim.Stats(); st.Concurrency != 2 || st.BatchIntervalSec != 20 {
		t.Fatalf("第 2 次 112 应到 2 / 20s，实际 %+v", st)
	}
	lim.Observe(gameproto.RegErrServerFail)
	if st = lim.Stats(); st.Concurrency != 2 || st.BatchIntervalSec != 40 {
		t.Fatalf("并发到下限 2 后不再降（间隔继续加倍到 40s），实际 %+v", st)
	}
	lim.Observe(gameproto.RegErrServerFail)
	if st = lim.Stats(); st.Concurrency != 2 || st.BatchIntervalSec != 60 {
		t.Fatalf("批间隔上限应夹在 60s，实际 %+v", st)
	}
	if st.Total112 != 4 {
		t.Fatalf("累计 112 应为 4，实际 %+v", st)
	}
}

// 连续成功 CreateRecoverAfter 次 → 回升一步（并发 +1、批间隔减半），回到配置上限止。
func TestCreateThrottleRecoversAfterSuccessStreak(t *testing.T) {
	lim := api.NewCreateThrottle(rateCfg())
	lim.Observe(gameproto.RegErrServerFail)
	lim.Observe(gameproto.RegErrServerFail) // 降到 2 / 20

	for i := 0; i < api.CreateRecoverAfter-1; i++ {
		lim.Observe(0)
	}
	st := lim.Stats()
	if st.Concurrency != 2 || st.BatchIntervalSec != 20 {
		t.Fatalf("连续成功不足 %d 次不该回升，实际 %+v", api.CreateRecoverAfter, st)
	}
	if st.TotalOK != api.CreateRecoverAfter-1 || st.Streak112 != 0 {
		t.Fatalf("成功应清零 112 连击并累加，实际 %+v", st)
	}

	lim.Observe(0) // 第 10 次成功 → 回升
	if st = lim.Stats(); st.Concurrency != 3 || st.BatchIntervalSec != 10 {
		t.Fatalf("第 %d 次成功应升到 3 / 10s，实际 %+v", api.CreateRecoverAfter, st)
	}
	for i := 0; i < 2*api.CreateRecoverAfter; i++ {
		lim.Observe(0) // 再过两轮：3→4→5
	}
	if st = lim.Stats(); st.Concurrency != 5 || st.BatchIntervalSec != 5 {
		t.Fatalf("批间隔应回到基准 5s（并发继续逐级升），实际 %+v", st)
	}
	for i := 0; i < 30*api.CreateRecoverAfter; i++ {
		lim.Observe(0)
	}
	if st = lim.Stats(); st.Concurrency != 8 {
		t.Fatalf("并发应回到配置上限 8 止，实际 %+v", st)
	}
}

// 中性错误（网络/协议失败）不算成功：中断"连续成功"，但也不降速。
func TestCreateThrottleNeutralErrorBreaksStreak(t *testing.T) {
	lim := api.NewCreateThrottle(rateCfg())
	lim.Observe(gameproto.RegErrServerFail) // 4 / 10
	for i := 0; i < api.CreateRecoverAfter-1; i++ {
		lim.Observe(0)
	}
	lim.Observe(-1) // 中性错误：把连击打断
	lim.Observe(0)
	if st := lim.Stats(); st.Concurrency != 4 || st.BatchIntervalSec != 10 {
		t.Fatalf("连击被打断后不该回升，实际 %+v", st)
	}
}

// 关掉自适应：只统计，不改节奏（抖动仍由 Pace 决定）。
func TestCreateThrottleAdaptiveOffOnlyCounts(t *testing.T) {
	cfg := rateCfg()
	cfg.Adaptive = false
	lim := api.NewCreateThrottle(cfg)
	lim.Observe(gameproto.RegErrServerFail)
	lim.Observe(gameproto.RegErrServerFail)
	st := lim.Stats()
	if st.Concurrency != 8 || st.BatchIntervalSec != 5 {
		t.Fatalf("自适应关着时节奏不该变，实际 %+v", st)
	}
	if st.Total112 != 2 {
		t.Fatalf("自适应关着时仍应统计 112（可观测），实际 %+v", st)
	}
}

// 节奏：批间隔 = max(请求, 限速器) ± jitter；并发取 min(请求, 限速器)（请求只能更保守）。
func TestCreateThrottlePaceTakesConservativeSide(t *testing.T) {
	lim := api.NewCreateThrottle(rateCfg())
	// rnd 固定 0 → 抖动 -2s；基准 5s → 3s
	conc, gap := lim.Pace(8, 0, func(int) int { return 0 })
	if conc != 8 {
		t.Fatalf("请求 8 且限速器 8 → 并发 8，实际 %d", conc)
	}
	if gap != 3*time.Second {
		t.Fatalf("5s ± 抖动（-2s）应为 3s，实际 %v", gap)
	}
	// rnd 固定最大 → 抖动 +2s → 7s
	if _, gap = lim.Pace(8, 0, func(n int) int { return n - 1 }); gap != 7*time.Second {
		t.Fatalf("5s + 抖动（+2s）应为 7s，实际 %v", gap)
	}
	// 请求更保守：只给 1 并发 → 听请求的
	if conc, _ = lim.Pace(1, 0, func(int) int { return 0 }); conc != 1 {
		t.Fatalf("请求并发更小应听请求的（1），实际 %d", conc)
	}
	// 限速器降速后，请求再大也压得住；请求间隔更保守时以请求为准
	lim.Observe(gameproto.RegErrServerFail) // 4 / 10
	if conc, gap = lim.Pace(8, 12_000, func(int) int { return 0 }); conc != 4 || gap != 10*time.Second {
		t.Fatalf("应取 min(8,4)=4 与 max(12s,10s)-2s=10s，实际 conc=%d gap=%v", conc, gap)
	}
}

// HTTP 侧：建号响应带 throttle 统计（成功一次 → total_ok=1）。
func TestAccountsCreateResponseCarriesThrottle(t *testing.T) {
	env := newTestEnv(t, "")
	cg := &createGame{}
	gs := testsupport.StartGameServer(t, cg.handler())

	code, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{"robot0009300@xy3.com"}, "zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if code != http.StatusOK || body["created"] != float64(1) {
		t.Fatalf("建号应成功: %d %v", code, body)
	}
	th, ok := body["throttle"].(map[string]any)
	if !ok {
		t.Fatalf("建号响应应带 throttle 统计: %v", body)
	}
	// 请求里 concurrency 默认 2（低于上限 8）→ 有效并发 2；限速器**状态**仍是上限 8
	if th["concurrency"] != float64(8) {
		t.Fatalf("限速器状态并发应为配置上限 8，实际 %v", th)
	}
	if body["concurrency"] != float64(2) {
		t.Fatalf("实际使用的并发应为请求的 2，实际 %v", body["concurrency"])
	}
	if th["total_ok"] != float64(1) || th["total_112"] != float64(0) || th["streak_112"] != float64(0) {
		t.Fatalf("成功一次应记 total_ok=1 / 无 112，实际 %v", th)
	}
	if th["batch_interval_sec"] != float64(0) {
		t.Fatalf("测试环境批间隔基准为 0（假游戏服无风控），实际 %v", th)
	}
}

// HTTP 侧：注册回 112 → 有效并发真的减半（8→4→2 下限），且 msg 点明 112。
func TestAccountsCreateThrottleSlowsDownOn112(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, (&createGame{regErrID: gameproto.RegErrServerFail}).handler())

	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{"robot0009301@xy3.com"}, "zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if body["ok"] != false || body["created"] != float64(0) {
		t.Fatalf("被风控的注册不该算成功: %v", body)
	}
	th, _ := body["throttle"].(map[string]any)
	if th["concurrency"] != float64(4) || th["total_112"] != float64(1) || th["streak_112"] != float64(1) {
		t.Fatalf("撞 1 次 112 应把并发减半到 4，实际 %v", th)
	}
	results, _ := body["results"].([]any)
	r0, _ := results[0].(map[string]any)
	if r0["err_id"] != float64(gameproto.RegErrServerFail) {
		t.Fatalf("结果应带 errid=112: %v", r0)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "112") {
		t.Fatalf("提示应点明 112 风控: %v", body["msg"])
	}

	_, body2 := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{"robot0009302@xy3.com"}, "zone": gs.Addr, "interval_ms": 0,
	}, nil)
	th2, _ := body2["throttle"].(map[string]any)
	if th2["concurrency"] != float64(2) || th2["total_112"] != float64(2) {
		t.Fatalf("连续第 2 次 112 应降到下限 2，实际 %v", th2)
	}
}
