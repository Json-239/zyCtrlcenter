// 2026-09-29 P0-4：分享日常（神捕/烽火）"交付连拒 13 次后停止"的卡死码
// SHARE_DAILY_HANDIN_STUCK 要进恢复链 —— 计入当日卡死（churn 计数）+ 触发 reghost
// （下线 → 重登 → 按意图重派）。
//
// 背景（docs/04-测试/分析-20260929-烽火大唐任务链.md §4.3）：该码不在 isStuckCode 白名单，
// robot0005054 02:12 停止后**空转 8 小时**没有任何恢复动作；同期 5162/1175 有 RESTORE 记录，
// 证明恢复引擎本身在工作 —— 缺口就是白名单少收录了这个码。
//
// 与 SHARE_DAILY_DAILY_LIMIT（满额收工 = 正常，不进错误链、记满额表）区分：本用例反证
// HANDIN_STUCK 不会被记入满额表（done<limit 时必须留痕为"卡死"而不是"跑完"）。
package event_test

import (
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

func TestShareDailyHandinStuckCountsAndTriggersReghost(t *testing.T) {
	h, st, runStore, _ := testsupport.NewTestHandler(t, nil)
	type call struct{ account, reason string }
	var calls []call
	h.SetReghoster(func(account, reason string) { calls = append(calls, call{account, reason}) })

	const acc = "robot0005054@xy3.com"
	const key = "share_daily_宫廷10"
	const msg = "交付未被服务端接受(尝试 13 次, 交付NPC或任务状态不符)"

	// 机器人端事件形状：{type:"error", code:"SHARE_DAILY_<CODE>", msg, state:"STOPPED",
	// reason, share_key, done, limit}（share_daily.py __request_stop）。
	h.HandleEvent(map[string]any{"type": "error", "code": "SHARE_DAILY_HANDIN_STUCK",
		"account": acc, "msg": msg, "state": "STOPPED", "share_key": key, "done": 9, "limit": 20})

	r, ok := st.Get(acc)
	if !ok {
		t.Fatalf("error 后应建立账号行 %s", acc)
	}
	if r.StuckCount != 1 || r.StuckDay != time.Now().Format("20060102") {
		t.Fatalf("SHARE_DAILY_HANDIN_STUCK 应计入当日卡死计数，实际 stuck_count=%d stuck_day=%q",
			r.StuckCount, r.StuckDay)
	}
	if len(calls) != 1 || calls[0].account != acc || calls[0].reason != msg {
		t.Fatalf("SHARE_DAILY_HANDIN_STUCK 应触发一次重登恢复（reason=原文），实际 %+v", calls)
	}
	if !testsupport.StoreHasType(runStore, "error") {
		t.Fatal("卡死类错误也必须写运行历史")
	}
	// 反证：done(9) < limit(20) → 不是"满额收工"，绝不能记入满额表（否则次日之前都不会再派）
	if st.ShareDailyFullToday(acc, key) {
		t.Fatal("交付被拒（done<limit）不该被记成满额：满额表只由 DAILY_LIMIT 事件写入")
	}
	// 普通分享日常停止码（如 ACCEPT_REFUSED）不进卡死链 —— 口径边界：只收交付连拒这一类
	// "重登+重派可能自愈"的；接取被拒多为包满/条件不满足，走常规错误记账（ErrRepeat 封禁）。
	h.HandleEvent(map[string]any{"type": "error", "code": "SHARE_DAILY_ACCEPT_REFUSED",
		"account": acc, "msg": "接取任务失败: 包裹已满，请清理包裹"})
	r, _ = st.Get(acc)
	if r.StuckCount != 1 || len(calls) != 1 {
		t.Fatalf("ACCEPT_REFUSED 不应计入卡死/触发重登（防误救），实际 stuck=%d calls=%d",
			r.StuckCount, len(calls))
	}

	// 同日第二次 HANDIN_STUCK → 累计（reghost 侧按 ≥ChurnLimit 熔断的输入；event 层只负责计数）
	h.HandleEvent(map[string]any{"type": "error", "code": "SHARE_DAILY_HANDIN_STUCK",
		"account": acc, "msg": msg, "share_key": key, "done": 9, "limit": 20})
	r, _ = st.Get(acc)
	if r.StuckCount != 2 || len(calls) != 2 {
		t.Fatalf("第二次 HANDIN_STUCK 应累计为 2 并再触发一次恢复，实际 stuck=%d calls=%d",
			r.StuckCount, len(calls))
	}
}
