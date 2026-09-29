// 2026-09-29 重登停滞事故 A+C（event 侧）：
//
//	A：hello（机器人进程握手）标记离线时**清运行时忙态**，否则水位补号候选被 Busy 全滤（池空）；
//	C：把"刚清出的本区账号"交给注入的回调（壳层实现自动补一次批量上线）。
//
// 只动"本区 + 在线"行；其它区/已离线行不受影响；pong 等周期心跳不触发。
package event_test

import (
	"testing"

	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
)

func TestHelloClearsRuntimeBusyAndCallsRestartAutoAdd(t *testing.T) {
	h, st, _, _ := testsupport.NewTestHandler(t, nil)
	var got []string
	h.SetRestartAutoAdd(func(cleared []string) { got = append(got, cleared...) })

	zone := testsupport.TestZoneKey
	// 本区在线忙号：FIGHT + 游荡 + 抓鬼 enabled + 摆摊（都属"运行时忙态"）
	st.Update("busy@x.com", func(r *state.Robot) {
		r.Zone, r.Online, r.HS = zone, true, true
		r.State, r.Fight = "FIGHT", true
		r.Walk = map[string]any{"enabled": true, "mapid": 10}
		r.Ghost = map[string]any{"enabled": true, "done": 9, "limit": 50}
		r.Booth = map[string]any{"enabled": true, "state": "OPEN"}
	})
	// 本区**已离线**号：不该被计入 cleared、也不该被二次改写
	st.Update("off@x.com", func(r *state.Robot) { r.Zone, r.Online, r.State = zone, false, "FIGHT" })
	// 其它区在线号：多区部署下不动
	otherZone := zone + "-b"
	st.Update("other@x.com", func(r *state.Robot) { r.Zone, r.Online, r.State = otherZone, true, "NAV" })

	h.HandleEvent(map[string]any{"type": "hello", "robot_version": "t", "pid": 1, "_zone": zone})

	br, _ := st.Get("busy@x.com")
	if br.Online || br.State != "OFFLINE" || br.Fight || br.Walk != nil || br.GhostActive() || br.Booth != nil {
		t.Fatalf("hello 应清 busy 号的在线+运行时忙态，实际 %+v", br)
	}
	if br.Ghost["done"] != 9 || br.Ghost["limit"] != 50 {
		t.Fatalf("抓鬼进度（done/limit）应保留，实际 %v", br.Ghost)
	}
	off, _ := st.Get("off@x.com")
	if off.State != "FIGHT" {
		t.Fatal("已离线号不受 hello 影响（不被二次改写）")
	}
	oth, _ := st.Get("other@x.com")
	if !oth.Online || oth.State != "NAV" {
		t.Fatal("其它区号不动作")
	}
	if len(got) != 1 || got[0] != "busy@x.com" {
		t.Fatalf("restartAutoAdd 回调应收到刚清出的本区账号，实际 %v", got)
	}

	// 反例：pong（周期心跳）不触发清扫、不触发回调
	got = nil
	st.Update("busy2@x.com", func(r *state.Robot) { r.Zone, r.Online, r.State = zone, true, "FIGHT" })
	h.HandleEvent(map[string]any{"type": "pong", "_zone": zone})
	b2, _ := st.Get("busy2@x.com")
	if !b2.Online || len(got) != 0 {
		t.Fatalf("pong 不应触发清扫/回调（b2=%+v got=%v）", b2, got)
	}
}
