// ③ 假补位止血（2026-09-29）：在跑的日常号（ACCEPT/KILL + 活跃 daily）不再进候选消费名额；
// 会话已停（STOPPED，未满）的号仍可"续跑"进候选。
package api_test

import (
	"testing"
)

func TestShareDailyRunnerNotCandidate(t *testing.T) {
	// 在跑：ACCEPT + 活跃 daily → 不算候选（deficit 才是真缺）
	env := newTestEnv(t, "")
	addUsableAccount(t, env, "lvd_run@xy3.com", 45)
	feedRobot(t, env, "lvd_run@xy3.com", map[string]any{"state": "ACCEPT", "daily": map[string]any{
		"share_key": "share_daily_大唐神捕", "done": 0, "limit": 10, "state": "ACCEPT"}})
	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	if n, _ := cands["shenbu"].(float64); n != 0 {
		t.Fatalf("在跑的日常号不该进候选（会被反复重派），实际 %v", n)
	}

	// 对照：会话已停（STOPPED、未满）→ 仍进候选（"启动=恢复当前任务"的续跑口径不变）
	env2 := newTestEnv(t, "")
	addUsableAccount(t, env2, "lvd_stop@xy3.com", 45)
	feedRobot(t, env2, "lvd_stop@xy3.com", map[string]any{"state": "READY", "daily": map[string]any{
		"share_key": "share_daily_大唐神捕", "done": 0, "limit": 10, "state": "STOPPED"}})
	body2 := getJSON(t, env2.srv.URL+"/api/autotask")
	cands2, _ := body2["candidates"].(map[string]any)
	if n, _ := cands2["shenbu"].(float64); n != 1 {
		t.Fatalf("已停未满的号应可续跑进候选，实际 %v", n)
	}
}
