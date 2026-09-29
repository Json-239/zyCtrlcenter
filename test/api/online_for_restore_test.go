// ④ OnlineForRestore（restorer 离线补拉的壳层通路）：robot_manage add 带池内密码；
// 无密码/空参数 → 明确错误（不静默）。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

func TestOnlineForRestoreDispatches(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "ro_go@xy3.com"
	env.pool.Add([]string{acc}, "pwd-ro", testZoneAddr, "")

	n, err := env.api.OnlineForRestore([]string{acc})
	if err != nil || n != 1 {
		t.Fatalf("补拉应发出 1 个，实际 n=%d err=%v", n, err)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "robot_manage" || cmd["action"] != "add" {
		t.Fatalf("应为 robot_manage add，实际 %v", cmd)
	}
	payload, _ := cmd["accounts"].([]any)
	if len(payload) != 1 {
		t.Fatalf("应带 1 个账号，实际 %v", cmd["accounts"])
	}
	pair, _ := payload[0].([]any)
	if len(pair) != 2 || pair[0] != acc || pair[1] != "pwd-ro" {
		t.Fatalf("载荷应为 [账号,池内密码]，实际 %v", payload[0])
	}

	// 无密码：明确错误（与批量上线同口径）
	acc2 := "ro_nopwd@xy3.com"
	env.pool.Add([]string{acc2}, "", testZoneAddr, "")
	if n, err := env.api.OnlineForRestore([]string{acc2}); err == nil || n != 0 {
		t.Fatalf("无密码应报错不发出，实际 n=%d err=%v", n, err)
	}
	// 空参数
	if _, err := env.api.OnlineForRestore(nil); err == nil {
		t.Fatal("空参数应报错")
	}
}
