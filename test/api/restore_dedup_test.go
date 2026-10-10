// 2026-10-10（跨天假满额 Go 侧防线）：RESTORE 刚补发的号，直派候选不再重复派。
//
// 现场形态：恢复引擎补发 ghost_start 后 120s 内（机器人会话还没起来），批量上线/定时补号
// 又把同一号直派一遍 → 机器人端重启会话/吞配额。这里用真恢复引擎（POST /api/intents/restore
// → TickForce(force)）打出补发记录，再从 /api/autotask 候选侧验证该号被排除、其余号不受影响。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

func TestAutotaskCandidatesSkipFreshRestoreDispatch(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 10, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}

	acc, other := "rd1@xy3.com", "rd2@xy3.com"
	env.pool.Add([]string{acc, other}, "pwd", "", "")
	for _, a := range []string{acc, other} {
		env.pool.SetZoneState(a, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": other, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})
	// 只让 acc 留在意图表：TickForce 会对意图表里**所有**号记账（含被 handler 过滤掉的），
	// 对照号必须在意图表外，才能证明"只排除刚补发的那一个"。
	if !env.ev.Intents.Remove(other) {
		t.Fatalf("对照号应已登记意图（便于摘除）")
	}

	if got := candidatesOf(t, env, "ghost"); got != 2 {
		t.Fatalf("初始应有 2 个抓鬼候选，实得 %d", got)
	}

	// 恢复引擎（手动补发通道）给 acc 补发一份 ghost_start
	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{"account": acc}, nil)
	if res["ok"] != true || res["sent"] != float64(1) {
		t.Fatalf("应补发 1 条: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" || !cmdHasAccount(cmd, acc) {
		t.Fatalf("机器人应收到 %s 的 ghost_start: %v", acc, cmd)
	}

	// acc 已"刚补发、会话还没起来" → 直派候选排除它；对照号 other 不受影响
	if got := candidatesOf(t, env, "ghost"); got != 1 {
		t.Fatalf("刚补发的 %s 应被候选排除（只剩对照号），实得 %d", acc, got)
	}
}
