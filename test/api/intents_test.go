// 意图表接口：GET /api/intents（P0：判据 + 一个账号只有一条链）。
// 意图由事件驱动登记：账号上线（带等级）时判一次；新手链完成时转抓鬼。
package api_test

import (
	"testing"
)

func TestIntentsEndpointDecideFlow(t *testing.T) {
	env := newTestEnv(t, "")
	zone := testsupportZone()

	// 0) 一开始是空的，阈值按配置回带（默认 31）
	body := getJSON(t, env.srv.URL+"/api/intents")
	if body["ok"] != true || body["count"] != float64(0) {
		t.Fatalf("初始应为空: %v", body)
	}
	if body["newbie_max_level"] != float64(31) {
		t.Fatalf("应回带 newbie_max_level=31: %v", body["newbie_max_level"])
	}

	// 1) 20 级上线 → 判新手链优先
	env.ev.HandleEvent(map[string]any{"type": "robot_online", "account": "robot0007000@xy3.com",
		"role_name": "小号", "level": 20, "mapid": 11, "_zone": zone})
	body = getJSON(t, env.srv.URL+"/api/intents")
	if body["count"] != float64(1) {
		t.Fatalf("上线后应登记 1 条意图: %v", body)
	}
	items, _ := body["intents"].([]any)
	it0, _ := items[0].(map[string]any)
	if it0["kind"] != "newbie" || it0["chain_id"] != "newbie_full" {
		t.Fatalf("20 级应判新手链: %v", it0)
	}
	if it0["reason"] == "" || it0["source"] != "decide" {
		t.Fatalf("要带理由与来源（面板/排障要看）: %v", it0)
	}

	// 2) 升到 45 级再上线 → 切到抓鬼（同一账号仍只有一条链）
	env.ev.HandleEvent(map[string]any{"type": "robot_online", "account": "robot0007000@xy3.com",
		"role_name": "大号", "level": 45, "mapid": 11, "_zone": zone})
	body = getJSON(t, env.srv.URL+"/api/intents")
	items, _ = body["intents"].([]any)
	if len(items) != 1 {
		t.Fatalf("一个账号只应有一条意图: %v", items)
	}
	it0, _ = items[0].(map[string]any)
	if it0["kind"] != "ghost" {
		t.Fatalf("45 级应判抓鬼: %v", it0)
	}
	counts, _ := body["counts"].(map[string]any)
	if counts["newbie"] != nil || counts["ghost"] != float64(1) {
		t.Fatalf("计数应按 kind 汇总: %v", counts)
	}

	// 3) 新手链完成事件 → 仍判抓鬼（等级低也一样，别重复跑）
	env.ev.HandleEvent(map[string]any{"type": "chain_done", "account": "robot0007000@xy3.com",
		"chain_id": "newbie_full", "done": 118, "_zone": zone})
	body = getJSON(t, env.srv.URL+"/api/intents")
	items, _ = body["intents"].([]any)
	it0, _ = items[0].(map[string]any)
	if it0["kind"] != "ghost" {
		t.Fatalf("链完成后应是抓鬼: %v", it0)
	}

	// 4) 下线不删意图（P1 恢复引擎要用它把号重新拉起）
	env.ev.HandleEvent(map[string]any{"type": "robot_offline", "account": "robot0007000@xy3.com", "_zone": zone})
	body = getJSON(t, env.srv.URL+"/api/intents")
	if body["count"] != float64(1) {
		t.Fatalf("下线应保留意图（供恢复）: %v", body)
	}
}

// 等级未知（机器人没报 level）→ 不瞎判、不登记。
func TestIntentsPendingWhenLevelUnknown(t *testing.T) {
	env := newTestEnv(t, "")
	env.ev.HandleEvent(map[string]any{"type": "robot_online", "account": "robot0007001@xy3.com",
		"_zone": testsupportZone()})
	body := getJSON(t, env.srv.URL+"/api/intents")
	if body["count"] != float64(0) {
		t.Fatalf("等级未知时不该登记意图（等报一次等级）: %v", body)
	}
	if body["ok"] != true {
		t.Fatalf("接口本身要正常返回: %v", body)
	}
}

// 重连后机器人不会再发一次 robot_online：心跳里带到等级/链完成时也要判一次，
// 否则中控重启后意图表会一直是空的（参考实现用 intent_restore 覆盖这段）。
func TestIntentsDecidedFromStatusHeartbeat(t *testing.T) {
	env := newTestEnv(t, "")
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "robot0007002@xy3.com", "level": 18, "chain_done": false,
			"online": true, "state": "WAIT_TASK",
		}},
		"_zone": testsupportZone()})

	body := getJSON(t, env.srv.URL+"/api/intents")
	items, _ := body["intents"].([]any)
	if len(items) != 1 {
		t.Fatalf("心跳里带等级也应登记意图（重连场景）: %v", body)
	}
	it0, _ := items[0].(map[string]any)
	if it0["kind"] != "newbie" || it0["source"] != "status" {
		t.Fatalf("18 级应判新手链且 source=status: %v", it0)
	}
}

// 测试环境的区标记（与 testsupport.NewTestChannel 的 tag 一致）。
func testsupportZone() string {
	return "test-srv/z1"
}
