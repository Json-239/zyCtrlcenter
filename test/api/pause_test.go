// 2026-09-23 R3/R4（操作健壮性审计修复）：人工暂停闸 + 批量下线取消重登。
//
// R3：面板点「停止/停链」= 人工暂停 —— 恢复引擎/定时任务/水位/游荡池派发前跳过暂停号，
// 直到用户再次「启动 / 立即补发 / 上线」清标（生产 CTRL_AUTO_RESTORE=1，不打标会在
// 几秒内被按意图补发拉起，用户观感"停不住"）。
// R4：批量下线统一取消 reghost —— 否则当日卡死过的号刚下线就被自动重登拉起。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// R3：停止 → 打暂停标（/api/status 可见）→ 三道派发闸全拦 → 再「启动」解除。
func TestStopPausesAndBlocksDispatch(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	const acc = "p1@xy3.com"
	env.pool.Add([]string{acc}, "pwd", "", "")
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})
	if got := candidatesOf(t, env, "ghost"); got != 1 {
		t.Fatalf("暂停前应有 1 个抓鬼候选，实得 %d", got)
	}

	// 停止：打暂停标 + 下发 stop
	_, body := postJSON(t, env.srv.URL+"/api/stop", map[string]any{
		"accounts": []string{acc},
	}, nil)
	if body["ok"] != true {
		t.Fatalf("停链应成功: %v", body)
	}
	if n, _ := body["paused"].(float64); n != 1 {
		t.Fatalf("应标 1 个人工暂停: %v", body)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["cmd"] != "stop" {
		t.Fatalf("应收 stop 命令: %v", cmd)
	}

	// 面板可见：顶层 paused 列表 + robots[].paused
	st := getJSON(t, env.srv.URL+"/api/status")
	if !listHas(st["paused"], acc) {
		t.Fatalf("/api/status.paused 应含 %s: %v", acc, st["paused"])
	}
	found := false
	for _, v := range asSlice(st["robots"]) {
		m, _ := v.(map[string]any)
		if toStrAny(m["account"]) == acc {
			found = true
			if m["paused"] != true {
				t.Fatalf("robots 行应带 paused=true: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("/api/status 里应还有该号行（暂停不清行）: %v", st["robots"])
	}

	// 闸门 1：恢复引擎补发闸
	if blocked, why := env.api.GhostSkipFunc()("ghost", acc); !blocked || !containsText(why, "人工暂停") {
		t.Fatalf("暂停号应被补发闸拦下并说明原因，实得 blocked=%v why=%q", blocked, why)
	}
	// 闸门 2：LaunchTask（定时补号 / 卡死重登恢复共用）
	if ok, msg := env.api.LaunchTask(autotask.KindGhost, []string{acc}); ok || !containsText(msg, "人工暂停") {
		t.Fatalf("暂停号不该被 LaunchTask 派发，实得 ok=%v msg=%q", ok, msg)
	}
	// 闸门 3：定时任务候选排除
	if got := candidatesOf(t, env, "ghost"); got != 0 {
		t.Fatalf("暂停号应被候选排除，实得候选 %d", got)
	}
	if extra := rb.TryReadCmd(200 * time.Millisecond); extra != nil {
		t.Fatalf("暂停期间不该有下行命令，实收: %v", extra)
	}

	// 解除：显式「启动(自动分配)」→ 清标 + 正常派发
	_, body2 := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{acc},
	}, nil)
	groups, _ := body2["groups"].(map[string]any)
	if got := len(asSlice(groups["ghost_start"])); got != 1 {
		t.Fatalf("启动应解除暂停并派 1 个，实派 %d（msg=%v）", got, body2["msg"])
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["cmd"] != "ghost_start" {
		t.Fatalf("解除后应收 ghost_start: %v", cmd)
	}
	st2 := getJSON(t, env.srv.URL+"/api/status")
	if listHas(st2["paused"], acc) {
		t.Fatalf("启动后应解除人工暂停: %v", st2["paused"])
	}
}

// R3：手动「立即补发」= 用户要它跑 → 先解除暂停，再按意图补发（否则会被暂停闸全拦）。
func TestIntentsRestoreResumesPause(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	// restorer 的补发闸会看池状态（池未启用/已达标不补）→ 起一个启用且有缺口的抓鬼池。
	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 10, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}

	const acc = "p2@xy3.com"
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})

	// 先暂停
	if _, body := postJSON(t, env.srv.URL+"/api/stop", map[string]any{
		"accounts": []string{acc},
	}, nil); body["ok"] != true {
		t.Fatalf("停链应成功: %v", body)
	}
	_ = rb.ReadCmd(t, 2*time.Second) // stop

	// 暂停状态下点「立即补发」→ 先清标再补发（用户显式操作=解除暂停），sent=1 且收到 ghost_start
	_, restored := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{"account": acc}, nil)
	if n, _ := restored["sent"].(float64); n != 1 {
		t.Fatalf("「立即补发」应解除暂停并补发 1 个，实得 sent=%v（msg=%v）", restored["sent"], restored["msg"])
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["cmd"] != "ghost_start" {
		t.Fatalf("补发应收 ghost_start: %v", cmd)
	}
	if st := getJSON(t, env.srv.URL+"/api/status"); listHas(st["paused"], acc) {
		t.Fatalf("补发后应解除人工暂停: %v", st["paused"])
	}
}

// R4：批量下线统一取消 reghost —— 待恢复的号被下线后不该再被自动重登拉起。
func TestBatchOfflineCancelsReghost(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	const acc = "stuck@xy3.com"
	env.api.Reghost.Request(acc, "测试：钟馗对话卡死")
	_ = rb.TryReadCmd(500 * time.Millisecond) // Request 会立刻发一条 robot_manage remove（不校验）
	if !reghostHas(env, acc) {
		t.Fatalf("待恢复列表应登记该号: %v", env.api.Reghost.Status())
	}

	// 批量下线（显式给号，避免依赖在线状态）
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "offline", "accounts": []string{acc},
	}, nil)
	if body["ok"] != true {
		t.Fatalf("下线下发应成功: %v", body)
	}
	_ = rb.TryReadCmd(500 * time.Millisecond) // 下线命令 robot_manage remove
	if reghostHas(env, acc) {
		t.Fatalf("批量下线应取消待恢复（否则会被重登拉起），实际仍有: %v", env.api.Reghost.Status())
	}
}

// ---------------------------------------------------------------- 小工具

// listHas JSON 数组里是否含某字符串。
func listHas(v any, s string) bool {
	for _, it := range asSlice(v) {
		if toStrAny(it) == s {
			return true
		}
	}
	return false
}

// reghostHas 待恢复列表里是否有该号。
func reghostHas(env *testEnv, acc string) bool {
	if env.api.Reghost == nil {
		return false
	}
	for _, st := range env.api.Reghost.Status() {
		if st.Account == acc {
			return true
		}
	}
	return false
}
