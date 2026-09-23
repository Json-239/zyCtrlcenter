// 2026-09-23 文案修复（用户现场）：大屏/账号池点「启动」，若号被**池配额/在途闸门**拦下，
// 面板却提示"下发失败：机器人通道未连接" —— 这是 `okMsg(ok, ...)` 兜底文案的误用：
// sent==0 被一律当成"通道故障"。修复口径：
//   ① 真·传输失败（无连接/写失败）→ 保持"下发失败：机器人通道未连接"；
//   ② 被闸门拦下（配额/在途/门槛）→ 回真实原因（assignments[].reason 汇总）；
//   ③ 没有需要启动的号 → 回"没有需要启动的账号"。
// 本测试同时钉住两点：**拦下时不得出现"通道未连接"**、**真断连时必须是"通道未连接"**。
package api_test

import (
	"fmt"
	"strings"
	"testing"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// 需求①：被池配额/在途拦下 → msg 给真实原因，且 channel_fail=false。
func TestStartAutoBlockedMsgIsHonest(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 60, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	accs := []string{}
	robots := []any{}
	for i := 0; i < 4; i++ {
		acc := fmt.Sprintf("msgfix%d@xy3.com", i)
		accs = append(accs, acc)
		robots = append(robots, map[string]any{
			"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": robots, "_zone": testsupportZone()})

	// 第一次：池目标 2 → 派 2 个（应成功文案）
	_, b1 := postJSON(t, env.srv.URL+"/api/start", map[string]any{"auto": true, "accounts": accs}, nil)
	if b1["ok"] != true || b1["channel_fail"] == true {
		t.Fatalf("第一次应按配额派 2 个且非通道失败: %v", b1)
	}

	// 第二次：这 2 个还在途（TTL 内）+ 目标已满 → 一个都不派 → 必须是"真实原因"文案
	_, b2 := postJSON(t, env.srv.URL+"/api/start", map[string]any{"auto": true, "accounts": accs}, nil)
	if b2["sent"] != float64(0) {
		t.Fatalf("在途占满配额 → 不该再派: %v", b2)
	}
	if b2["channel_fail"] == true {
		t.Fatalf("被闸门拦下不该标记成通道失败: %v", b2)
	}
	m2, _ := b2["msg"].(string)
	if strings.Contains(m2, "通道未连接") {
		t.Fatalf("被闸门拦下却报'通道未连接'（本次修复的 bug）: %v", m2)
	}
	if !strings.Contains(m2, "本次未下发") {
		t.Fatalf("msg 应说明真实原因（配额/在途）: %v", m2)
	}
	// assignments 汇总出来的原因也要带上（面板能看到"为什么没派"）
	if !strings.Contains(m2, "配额") && !strings.Contains(m2, "在途") {
		t.Fatalf("msg 应含具体闸门原因: %v", m2)
	}
}

// 需求②：真·断连（机器人连接已关）→ 文案必须是"下发失败：机器人通道未连接"+channel_fail=true。
//
// 用新手链意图的号（新手池未配目标 → 不限，不拦）来走到真正的 SendCmd 失败分支。
func TestStartAutoRealChannelFailMsg(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	// 低等级 → 新手链意图；随后关掉连接制造"真失败"
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "chanfail@xy3.com", "level": 20, "online": true, "state": "IDLE", "task_index": 0}},
		"_zone": testsupportZone()})
	rb.Close() // 断开机器人通道

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"chanfail@xy3.com"},
	}, nil)
	if body["sent"] != float64(0) {
		t.Fatalf("通道断开时不该有 sent: %v", body)
	}
	if body["channel_fail"] != true {
		t.Fatalf("真断连应标记 channel_fail=true: %v", body)
	}
	m, _ := body["msg"].(string)
	if !strings.Contains(m, "通道未连接") {
		t.Fatalf("真断连应回'通道未连接': %v", m)
	}
}
