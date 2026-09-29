// 2026-09-29 重登停滞事故 C 侧：机器人重启握手后的"自动补一次批量上线"。
//
// 语义要点：开关 CTRL_RESTART_AUTO_ADD（默认开）；只补"刚被 hello 清出的账号"；
// 跳过人工暂停/已移除；无密码号跳过（密码只从池里取）；10 分钟冷却防重启风暴；
// 复用 sendOnlineChunks（robot_manage add）同一条分批通路。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

// 开关关 → 不补（无命令）；开关开但全部暂停/已移除 → 过滤后无可补（无命令）。
func TestRestartAutoAddSwitchAndFilter(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "ra_off@xy3.com"
	env.pool.Add([]string{acc}, "pwd-off", testZoneAddr, "")

	// ① 开关关：直接返回，不产生任何命令
	env.cfg.RestartAutoAdd = false
	env.api.OnRobotRestartHello([]string{acc})
	if cmd := rb.TryReadCmd(400 * time.Millisecond); cmd != nil {
		t.Fatalf("开关关时不该下发任何命令，实际 %v", cmd)
	}

	// ② 开关开但被过滤（暂停 + 已移除）→ 无可补
	env.cfg.RestartAutoAdd = true
	paused, removed := "ra_paused@xy3.com", "ra_removed@xy3.com"
	env.st.MarkPaused(paused)
	env.st.Remove(removed)
	env.api.OnRobotRestartHello([]string{paused, removed})
	if cmd := rb.TryReadCmd(400 * time.Millisecond); cmd != nil {
		t.Fatalf("暂停/已移除的号不该被自动拉起，实际 %v", cmd)
	}
}

// 成功路径：延迟后经 robot_manage add 下发（带池内密码；无密码号跳过）；
// 10 分钟内再次 hello 被冷却跳过；无密码号在延迟后不得单独下发。
func TestRestartAutoAddDispatchesThenCooldown(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc, noPwd := "ra_go@xy3.com", "ra_nopwd@xy3.com"
	env.pool.Add([]string{acc}, "pwd-go", testZoneAddr, "")
	env.pool.Add([]string{noPwd}, "", testZoneAddr, "") // 空密码 → 应被跳过

	env.api.OnRobotRestartHello([]string{acc, noPwd})
	// 延迟 = restartAutoAddDelay(8s)；给足超时等命令
	cmd := rb.ReadCmd(t, 12*time.Second)
	if cmd["cmd"] != "robot_manage" || cmd["action"] != "add" {
		t.Fatalf("应为 robot_manage add，实际 %v", cmd)
	}
	payload, _ := cmd["accounts"].([]any)
	if len(payload) != 1 {
		t.Fatalf("只该带 1 个账号（无密码号被跳过），实际 %v", cmd["accounts"])
	}
	pair, _ := payload[0].([]any)
	if len(pair) != 2 || pair[0] != acc || pair[1] != "pwd-go" {
		t.Fatalf("载荷应为 [账号,池内密码]，实际 %v", payload[0])
	}

	// 冷却：同实例再次 hello 应立即跳过（不产生第二条命令；也无"无密码号"的补发）
	env.api.OnRobotRestartHello([]string{acc, noPwd})
	if cmd2 := rb.TryReadCmd(700 * time.Millisecond); cmd2 != nil {
		t.Fatalf("冷却期内不该重发补号命令，实际 %v", cmd2)
	}
}
