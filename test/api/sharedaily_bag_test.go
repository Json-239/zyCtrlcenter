// 2026-09-29 P0-2 背包预检（日常池候选口径）：心跳 bag 近似满（≥50 条）的号不进
// shenbu/fenghuo 候选 —— 现场 6/9 个烽火号死在"采购/接取被拒：背包空格不足"，
// 派下去只会连拒后 HANDIN_STUCK 停止（见 docs/04-测试/分析-20260929-烽火大唐任务链.md §4.2）。
//
// 差分断言（同环境只改 bag 一个变量）：同是抓鬼满额、45 级的两个号 ——
// 一个 bag 达阈值被剔除、另一个照常进候选；49 条（<阈值）不误伤。
package api_test

import (
	"testing"
	"time"
)

func TestDailyCandidatesSkipNearFullBag(t *testing.T) {
	env := newTestEnv(t, "")
	accFull, accOK := "bagfull@xy3.com", "bagok@xy3.com"
	addUsableAccount(t, env, accFull, 45)
	addUsableAccount(t, env, accOK, 45)

	// 抓鬼满额（50/50 心跳）→ 日常候选的"抓鬼让路"豁免成立；号保持 IDLE（非在跑）。
	feedGhostFull := func(acc string, extra map[string]any) {
		t.Helper()
		st := map[string]any{"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
			"count_date": time.Now().Format("20060102")}}
		for k, v := range extra {
			st[k] = v
		}
		feedRobot(t, env, acc, st)
	}
	candShenbu := func() float64 {
		t.Helper()
		res := getJSON(t, env.srv.URL+"/api/autotask")
		cands, _ := res["candidates"].(map[string]any)
		n, _ := cands["shenbu"].(float64)
		return n
	}
	mkBag := func(n int) []any {
		bag := make([]any, n)
		for i := range bag {
			bag[i] = map[string]any{"id": float64(i + 1), "count": 1, "name": "杂物"}
		}
		return bag
	}

	// 基线：两个都进候选（抓鬼满额 + 45 级 + 无在跑）
	feedGhostFull(accFull, nil)
	feedGhostFull(accOK, nil)
	if n := candShenbu(); n != 2 {
		t.Fatalf("基线应 2 个候选，实际 %v", n)
	}

	// accFull 心跳 bag 达阈值（50 条）→ 只被剔除它（差分证明是背包闸，不是别的条件）
	feedGhostFull(accFull, map[string]any{"bag": mkBag(50)})
	if n := candShenbu(); n != 1 {
		t.Fatalf("近满包号应被候选剔除（剩 1 个），实际 %v", n)
	}

	// 49 条（<阈值 50）不误伤
	feedGhostFull(accFull, map[string]any{"bag": mkBag(49)})
	if n := candShenbu(); n != 2 {
		t.Fatalf("49 条 < 阈值不该拦（应回到 2 个候选），实际 %v", n)
	}

	// 空包（清空）照常
	feedGhostFull(accFull, map[string]any{"bag": []any{}})
	if n := candShenbu(); n != 2 {
		t.Fatalf("空包不该拦，实际 %v", n)
	}
}
