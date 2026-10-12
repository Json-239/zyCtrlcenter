// 号池轮换（rotation 最小版，2026-10-12）接口黑盒：
//   - GET /api/rotation：默认关的参数（per_hour/min_reserve/keep_hours）、预备队/名单数/今日下线数；
//   - POST /api/robots/unremove：单号/批量/幂等/空参数（→ 指向 clear_removed）；带 requireToken；
//   - POST /api/robots/clear_removed 回归：unremoveAccounts 公共实现重构后行为不变（全清）。
//
// 选择 test/api（外黑盒）的原因：这两条路由是纯 HTTP 契约，黑盒即可覆盖；
// tick 内部（注入时刻/首见在线表/下线通路）在 internal/api/rotation_test.go（内部包）覆盖。
package api_test

import (
	"net/http"
	"testing"
)

func TestRotationStatusDefaults(t *testing.T) {
	env := newTestEnv(t, "")
	body := getJSON(t, env.srv.URL+"/api/rotation")
	if body["ok"] != true {
		t.Fatalf("rotation 应返回 ok=true：%v", body)
	}
	if body["enabled"] != false {
		t.Fatalf("默认应关闭（CTRL_ROTATION=0）：%v", body["enabled"])
	}
	if body["per_hour"] != float64(30) || body["min_reserve"] != float64(50) || body["keep_hours"] != float64(24) {
		t.Fatalf("默认参数不符：per_hour=%v min_reserve=%v keep_hours=%v",
			body["per_hour"], body["min_reserve"], body["keep_hours"])
	}
	if body["list_count"] != float64(0) || body["offlined_today"] != float64(0) {
		t.Fatalf("空名单应 0：%v / %v", body["list_count"], body["offlined_today"])
	}
	// 预备队/上一轮动作字段存在（数值本身取决于池与当前区，不断言具体值）
	if _, ok := body["reserve"].(float64); !ok {
		t.Fatalf("应透出预备队数（reserve）：%v", body["reserve"])
	}
	if _, ok := body["last_action"]; !ok {
		t.Fatal("应透出上一轮动作（last_action）")
	}
	if body["max_per_tick"] != float64(10) || body["min_online_min"] != float64(30) {
		t.Fatalf("每轮上限/在线门槛口径不符：%v / %v", body["max_per_tick"], body["min_online_min"])
	}
}

func TestRobotsUnremoveSingleAndBatch(t *testing.T) {
	env := newTestEnv(t, "")

	// 单号：在移除名单里 → 解除 1 个
	env.st.MarkRemoved("robot0009001@xy3.com")
	_, body := postJSON(t, env.srv.URL+"/api/robots/unremove",
		map[string]any{"accounts": []string{"robot0009001@xy3.com"}}, nil)
	if body["ok"] != true || body["count"] != float64(1) {
		t.Fatalf("单号解除应 count=1：%v", body)
	}
	if env.st.IsRemoved("robot0009001@xy3.com") {
		t.Fatal("解除后不应还在移除名单")
	}
	// 幂等：再来一次 → count=0（不报错）
	_, body = postJSON(t, env.srv.URL+"/api/robots/unremove",
		map[string]any{"accounts": []string{"robot0009001@xy3.com"}}, nil)
	if body["ok"] != true || body["count"] != float64(0) {
		t.Fatalf("重复解除应幂等 count=0：%v", body)
	}

	// 批量：只数"实际在名单里"的号
	env.st.MarkRemoved("a@xy3.com")
	env.st.MarkRemoved("b@xy3.com")
	_, body = postJSON(t, env.srv.URL+"/api/robots/unremove",
		map[string]any{"accounts": []string{"a@xy3.com", "nobody@xy3.com", "b@xy3.com"}}, nil)
	if body["count"] != float64(2) {
		t.Fatalf("批量应只解除在名单里的 2 个：%v", body)
	}
	got, _ := body["accounts"].([]any)
	if len(got) != 2 || got[0] != "a@xy3.com" || got[1] != "b@xy3.com" {
		t.Fatalf("返回清单应升序且只含实际解除号：%v", got)
	}
	if env.st.IsRemoved("a@xy3.com") || env.st.IsRemoved("b@xy3.com") {
		t.Fatal("批量解除后不应还在移除名单")
	}

	// 空 accounts → 明确拒绝（全清语义走 clear_removed）
	_, body = postJSON(t, env.srv.URL+"/api/robots/unremove", map[string]any{}, nil)
	if body["ok"] != false {
		t.Fatalf("空 accounts 应拒绝并提示：%v", body)
	}
	if msg, _ := body["msg"].(string); msg == "" {
		t.Fatalf("应给出提示：%v", body)
	}

	// 回归：clear_removed 仍是全清（公共实现重构后行为不变）
	env.st.MarkRemoved("c@xy3.com")
	_, clr := postJSON(t, env.srv.URL+"/api/robots/clear_removed", map[string]any{}, nil)
	if clr["ok"] != true || clr["count"].(float64) < 1 {
		t.Fatalf("clear_removed 应仍能全清：%v", clr)
	}
	if env.st.IsRemoved("c@xy3.com") {
		t.Fatal("clear_removed 后不应还在移除名单")
	}
}

func TestRobotsUnremoveTokenAndReadOnly(t *testing.T) {
	env := newTestEnv(t, "secret-token")

	// 写接口：无 token → 401
	code, _ := postJSON(t, env.srv.URL+"/api/robots/unremove",
		map[string]any{"accounts": []string{"x@xy3.com"}}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unremove 未带 token 应 401，实际 %d", code)
	}
	// 带 token → 放行（号不在名单 → count=0 但 ok=true）
	code, body := postJSON(t, env.srv.URL+"/api/robots/unremove",
		map[string]any{"accounts": []string{"x@xy3.com"}},
		map[string]string{"X-API-Token": "secret-token"})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("带 token 应放行：%d %v", code, body)
	}

	// 只读接口：/api/rotation 不鉴权
	resp, err := http.Get(env.srv.URL + "/api/rotation")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotation 状态不应被鉴权拦截，实际 %d", resp.StatusCode)
	}
}
