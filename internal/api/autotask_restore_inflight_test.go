// 2026-10-10（跨天假满额 Go 侧防线）：RESTORE/AUTOTASK 双发去重 —— restoreInflightActive
// 单号判据 + 其纯函数内核（restoreRecordFresh / restoreSessionUp）的最小单测。
package api

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/restorer"
	"zyctrlcenter/internal/state"
)

// dispatchRestorer 驱动真 restorer 产生一条"已补发"记录（Kind/LastDispatchAt 落账）。
func dispatchRestorer(t *testing.T, acc string, kind intent.Kind, now time.Time) *restorer.Runner {
	t.Helper()
	r := restorer.New(restorer.Deps{
		Enabled:   func() bool { return true },
		ChannelUp: func() bool { return true },
		Intents: func() []intent.Intent {
			return []intent.Intent{{Account: acc, Kind: kind, Reason: "单测", Since: now.Unix()}}
		},
		Robot: func(a string) (state.Robot, bool) {
			return state.Robot{Account: acc, Online: true, State: "IDLE"}, a == acc
		},
		Now: func() time.Time { return now },
	})
	if acts := r.TickForce(now, true); len(acts) != 1 {
		t.Fatalf("驱动补发失败：期望 1 条决策，实得 %d（%+v）", len(acts), acts)
	}
	return r
}

func TestRestoreInflightActive(t *testing.T) {
	now := time.Now()
	acc := "ri1@xy3.com"
	r := dispatchRestorer(t, acc, intent.KindGhost, now)
	st := state.New()
	a := &API{Deps: Deps{St: st, Restorer: r}}

	if !a.restoreInflightActive(acc) {
		t.Fatalf("刚补发且会话未起来 → 应判在途")
	}
	if !a.restoreInflightActive(acc, autotask.KindGhost) {
		t.Fatalf("按 ghost 池过滤 → 应判在途")
	}
	if a.restoreInflightActive(acc, autotask.KindNewbie) {
		t.Fatalf("按 newbie 池过滤 → ghost 补发记录不算在途")
	}
	// 会话已起来（心跳 ghost.enabled=true）→ 不算在途
	st.Update(acc, func(rb *state.Robot) { rb.Ghost = map[string]any{"enabled": true} })
	if a.restoreInflightActive(acc) {
		t.Fatalf("活跃抓鬼会话已建立 → 不应再算在途")
	}
	// 其它号 / 空号 / 未装配 Restorer
	if a.restoreInflightActive("nobody@xy3.com") {
		t.Fatalf("无恢复记录的号不应判在途")
	}
	if a.restoreInflightActive("") {
		t.Fatalf("空账号不应判在途")
	}
	if b := (&API{Deps: Deps{St: st}}); b.restoreInflightActive(acc) {
		t.Fatalf("Restorer 缺席不应判在途")
	}
}

func TestRestoreInflightActiveDailyKind(t *testing.T) {
	now := time.Now()
	acc := "ri2@xy3.com"
	r := dispatchRestorer(t, acc, intent.KindShenbu, now)
	st := state.New()
	a := &API{Deps: Deps{St: st, Restorer: r}}

	if !a.restoreInflightActive(acc) {
		t.Fatalf("刚补发神捕、心跳未有 daily → 应判在途")
	}
	st.Update(acc, func(rb *state.Robot) {
		rb.Online = true
		rb.Daily = map[string]any{"share_key": a.shareDailyKey(), "done": 0, "limit": 10}
	})
	if a.restoreInflightActive(acc) {
		t.Fatalf("心跳 daily 已在跑 → 不算在途")
	}
}

func TestRestoreRecordFreshBoundary(t *testing.T) {
	now := time.Now()
	fresh := restorer.Status{Account: "ri3@xy3.com", Kind: string(intent.KindGhost), LastDispatchAt: now}
	if !restoreRecordFresh(fresh, now) {
		t.Fatalf("刚补发应 fresh")
	}
	if !restoreRecordFresh(fresh, now.Add(ghostInflightTTL)) {
		t.Fatalf("TTL 边界应 fresh（<=）")
	}
	if restoreRecordFresh(fresh, now.Add(ghostInflightTTL+time.Second)) {
		t.Fatalf("超 TTL 不应 fresh")
	}
	if restoreRecordFresh(restorer.Status{Account: "ri3@xy3.com"}, now) {
		t.Fatalf("LastDispatchAt 零值不应 fresh")
	}
}

func TestRestoreSessionUp(t *testing.T) {
	ghost := restorer.Status{Kind: string(intent.KindGhost)}
	if !restoreSessionUp(ghost, state.Robot{Ghost: map[string]any{"enabled": true}}, "") {
		t.Fatalf("ghost 活跃会话 → 已起来")
	}
	if restoreSessionUp(ghost, state.Robot{Online: true, State: "IDLE"}, "") {
		t.Fatalf("ghost 无会话 → 未起来")
	}
	nb := restorer.Status{Kind: string(intent.KindNewbie)}
	if !restoreSessionUp(nb, state.Robot{Online: true, State: "NAV"}, "") {
		t.Fatalf("链推进中 → 已起来")
	}
	if restoreSessionUp(nb, state.Robot{Online: true, State: "IDLE"}, "") {
		t.Fatalf("IDLE → 未起来")
	}
	if restoreSessionUp(nb, state.Robot{Online: false, State: "IDLE"}, "") {
		t.Fatalf("离线 → 未起来")
	}
	daily := restorer.Status{Kind: string(intent.KindShenbu)}
	if !restoreSessionUp(daily,
		state.Robot{Online: true, Daily: map[string]any{"share_key": "share_daily_大唐神捕", "done": 0, "limit": 10}},
		"share_daily_大唐神捕") {
		t.Fatalf("daily 在跑 → 已起来")
	}
	if restoreSessionUp(daily,
		state.Robot{Online: true, Daily: map[string]any{"share_key": "share_daily_大唐神捕", "done": 10, "limit": 10}},
		"share_daily_大唐神捕") {
		t.Fatalf("daily 已满 → 未起来")
	}
	if restoreSessionUp(daily, state.Robot{Online: true}, "") {
		t.Fatalf("无 daily key → 未起来")
	}
}
