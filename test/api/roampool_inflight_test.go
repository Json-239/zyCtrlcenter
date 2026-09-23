// 2026-09-23 派发节流降噪（Go 侧 P0-a/P1）的接口层用例：
//   - P0-a：定向派发显式带 maps=[目标图]（机器人端不再误判"给定选图全被排除"）；
//   - P1：游荡池参数 inflight_ttl_sec / backoff_sec 可写可读、非法值拒绝且不生效。
package api_test

import (
	"path/filepath"
	"testing"
	"time"

	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/test/testsupport"
)

// P0-a：定向派发带 maps=[目标图]；调用方显式给了白名单时**原样保留**（不覆盖）；
// 随机图不受影响（白名单只来自调用方）。
func TestRandomWalkDirectedMapsGate(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "rw_gate@xy3.com"
	feedRobot(t, env, acc, nil)

	// ① 定向派发（不带 maps）→ 命令里补 maps=[6]
	_, body := postJSON(t, env.srv.URL+"/api/random_walk",
		map[string]any{"accounts": []string{acc}, "mapid": 6}, nil)
	if body["ok"] != true {
		t.Fatalf("定向派发应成功: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if got := asSlice(cmd["maps"]); len(got) != 1 || got[0] != float64(6) {
		t.Fatalf("定向派发应补 maps=[6]，实际 %v", cmd["maps"])
	}

	// ② 显式白名单（含目标图）→ 原样保留，不被改成 [6]
	_, body2 := postJSON(t, env.srv.URL+"/api/random_walk",
		map[string]any{"accounts": []string{acc}, "mapid": 6, "maps": []any{6, 10}}, nil)
	if body2["ok"] != true {
		t.Fatalf("带白名单的定向派发应成功: %v", body2)
	}
	cmd2 := rb.ReadCmd(t, 2*time.Second)
	if got := asSlice(cmd2["maps"]); len(got) != 2 {
		t.Fatalf("显式白名单应原样保留（2 项），实际 %v", cmd2["maps"])
	}
}

// P1：游荡池新增参数（在途 TTL / 退避档位）可写可读；非法值明确拒绝且不生效。
func TestRoampoolInflightParams(t *testing.T) {
	env := newTestEnv(t, "")
	env.api.Roampool = roampool.New(filepath.Join(t.TempDir(), "roampool.json"), env.api.RoampoolDeps())

	_, body := postJSON(t, env.srv.URL+"/api/roampool",
		map[string]any{"enabled": true, "inflight_ttl_sec": 90, "backoff_sec": []any{30, 60, 120}}, nil)
	if body["ok"] != true {
		t.Fatalf("设置应成功: %v", body)
	}
	got := getJSON(t, env.srv.URL+"/api/roampool")
	if got["inflight_ttl_sec"] != float64(90) {
		t.Fatalf("inflight_ttl_sec 应回读 90: %v", got["inflight_ttl_sec"])
	}
	if bs := asSlice(got["backoff_sec"]); len(bs) != 3 || bs[0] != float64(30) {
		t.Fatalf("backoff_sec 应回读 [30,60,120]: %v", got["backoff_sec"])
	}

	// 非法退避档位（0 秒）→ 拒绝且不改内存参数
	_, bad := postJSON(t, env.srv.URL+"/api/roampool",
		map[string]any{"backoff_sec": []any{0}}, nil)
	if bad["ok"] != false {
		t.Fatalf("非法 backoff_sec 应被拒绝: %v", bad)
	}
	got2 := getJSON(t, env.srv.URL+"/api/roampool")
	if bs := asSlice(got2["backoff_sec"]); len(bs) != 3 {
		t.Fatalf("拒绝后不该改动已有参数: %v", got2["backoff_sec"])
	}

	// 在途明细字段暴露（空账 → 0 条 + pending=0）
	if got2["inflight_pending"] != float64(0) {
		t.Fatalf("没有派发时 inflight_pending 应为 0: %v", got2["inflight_pending"])
	}
}
