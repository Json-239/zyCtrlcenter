// 2026-09-24 生产死锁回归（事故报告：docs/04-测试/事故-20260924-调度器死锁.md）。
//
// 现场：10:18:52 面板启动「大唐神捕」后，AUTOTASK 轮询 / RESTORE 恢复引擎 / GET /api/autotask
// 全部永久挂起（/api/status、WATERLINE 正常）。根因：Runner 持 r.mu 回调 Candidates，而
// shenbu 候选判定里回读 a.AutoTask.States() —— 同 goroutine 对非重入锁重入 → 永久死锁。
//
// 本用例钉住该锁序：启用 shenbu 后①驱动一轮 Tick（与 Run 的 5s 轮询同路径）、②读面板接口、
// ③走恢复引擎的派发闸，全部必须在限时内返回。旧代码在①就永久卡死（测试超时即失败）。
package api_test

import (
	"net/http"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
)

func TestShenbuTickDoesNotDeadlockRunner(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com" // 夹具账号：该区 usable（心跳 45 级覆盖池内 36 级）
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
			"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
				"count_date": time.Now().Format("20060102")}}},
		"_zone": testsupportZone()})

	// 与生产同路径：面板 POST /api/autotask/start 启动 shenbu（= 10:18:52 那一次）
	_, res := postJSON(t, env.srv.URL+"/api/autotask/start", map[string]any{
		"kind": "shenbu", "interval_sec": 300, "target_online": 10,
		"min_level": 40, "balance_gate": 1000, "batch_min": 1, "batch_max": 3,
	}, nil)
	if res["ok"] != true {
		t.Fatalf("启动 shenbu 失败: %v", res)
	}

	// ① Runner 主循环同路径：Tick 必须在限时内返回（旧代码在此永久卡死）
	done := make(chan []autotask.Round, 1)
	go func() { done <- env.api.AutoTask.Tick(time.Now()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AUTOTASK Tick 死锁：shenbu 候选路径在持有 Runner 锁时回读 Runner 状态（锁重入）")
	}

	// ② 面板读接口不得被拖死（生产现象：GET /api/autotask 20s+ 无响应）
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(env.srv.URL + "/api/autotask")
	if err != nil {
		t.Fatalf("GET /api/autotask 被拖死（Runner 锁未释放）: %v", err)
	}
	defer resp.Body.Close()
	body := decodeBody(t, resp.Body)
	cands, _ := body["candidates"].(map[string]any)
	if cands["shenbu"] != float64(1) {
		t.Fatalf("45 级号（策略门槛 40）应进 shenbu 候选，实际 %v", cands)
	}

	// ③ 恢复引擎的派发闸（restorer Skip → poolAllowsDispatch → States）不得被拖死
	skipDone := make(chan struct{}, 1)
	go func() {
		env.api.GhostSkipFunc()("shenbu", acc)
		skipDone <- struct{}{}
	}()
	select {
	case <-skipDone:
	case <-time.After(5 * time.Second):
		t.Fatal("GhostSkipFunc 被拖死：RESTORE 恢复引擎会随之停摆（现场现象之一）")
	}
}
