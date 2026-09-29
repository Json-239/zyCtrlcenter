// ③ 假补位止血（2026-09-29）：分享日常"活跃会话"的 ACCEPT/KILL/READY 判忙。
//
// 背景：这些相位不在 Busy 通用白名单 → 在跑的神捕/烽火号被当"没在忙"，每轮被挑成候选
// 重复派（消费配额、deficit 失真）。判据用"有活跃日常会话"限定，避免 READY 被一刀切。
package waterline_test

import (
	"testing"

	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
)

func TestBusyShareDailyPhases(t *testing.T) {
	daily := func(stateName string, done, limit int) map[string]any {
		return map[string]any{"share_key": "share_daily_大唐神捕", "done": done, "limit": limit, "state": stateName}
	}
	// 活跃日常会话 + 三相位 → 忙
	for _, st := range []string{"ACCEPT", "KILL", "READY"} {
		r := state.Robot{Account: "a", Online: true, State: st, Daily: daily(st, 0, 10)}
		if !waterline.Busy(r) {
			t.Fatalf("%s + 活跃日常会话应判忙（假补位止血）", st)
		}
	}
	// 无 daily：同名状态不忙（READY/ACCEPT/KILL 不可一刀切；ACCEPT 用于抓鬼时另有 GhostActive）
	for _, st := range []string{"READY", "ACCEPT", "KILL"} {
		r := state.Robot{Account: "a", Online: true, State: st}
		if waterline.Busy(r) {
			t.Fatalf("%s 无日常会话不该判忙", st)
		}
	}
	// 停止的会话不算活跃
	if waterline.Busy(state.Robot{Account: "a", Online: true, State: "READY", Daily: daily("STOPPED", 0, 10)}) {
		t.Fatal("STOPPED 会话不该判忙")
	}
	// 满额（已收工）不算活跃
	if waterline.Busy(state.Robot{Account: "a", Online: true, State: "KILL", Daily: daily("KILL", 10, 10)}) {
		t.Fatal("满额(10/10)不该判忙")
	}
	// 离线号仍不算忙（B 项口径为最高优先）
	if waterline.Busy(state.Robot{Account: "a", Online: false, State: "KILL", Daily: daily("KILL", 0, 10)}) {
		t.Fatal("离线号不算忙（B 项豁免优先）")
	}
}
