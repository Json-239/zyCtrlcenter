// 私人池 Wave 1（event 侧，2026-09-30）：私号不产生自动意图 + 不自动下机 + hello 重推名单。
//
// 意图表是所有自动编排（候选/恢复/重登/轮转总览队列）的入口；removeAccount 是 chain_done/
// ghost_done/ghost_offline 的自动下机口。注入 SetPersonalPool/SetPersonalPush（main 装配）。
package event_test

import (
	"testing"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/test/testsupport"
)

func TestPersonalIntentVoidAndAutoRemoveExempt(t *testing.T) {
	cfg := config.Default() // AutoRemoveOnDone=true（默认）；对照组要能观察到自动下机
	h, st, _, _ := testsupport.NewTestHandler(t, cfg)
	h.SetPersonalPool(func(a string) bool { return a == "pv@x.com" })
	pushCalls := 0
	h.SetPersonalPush(func() { pushCalls++ })

	// ① 意图作废：同心跳下，私号无意图、正常号有意图（差分）
	h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "pv@x.com", "online": true, "state": "IDLE", "level": 45},
			map[string]any{"account": "nm@x.com", "online": true, "state": "IDLE", "level": 45},
		}, "_zone": testsupport.TestZoneKey})
	kinds := map[string]string{}
	for _, it := range h.Intents.Snapshot() {
		kinds[it.Account] = string(it.Kind)
	}
	if k, ok := kinds["pv@x.com"]; ok {
		t.Fatalf("私号不该产生自动意图，实际 %q", k)
	}
	if kinds["nm@x.com"] == "" {
		t.Fatal("正常号应照常有意图（对照）")
	}

	// ② 自动下机豁免：chain_done 事件对该号不下机；正常号为对照（被移除）
	clone := func(acc string) map[string]any {
		ev := testsupport.CloneEvent(t, testsupport.LoadFixture(t, "chain_done.json").Event)
		ev["account"] = acc
		return ev
	}
	h.HandleEvent(clone("pv@x.com"))
	if st.IsRemoved("pv@x.com") {
		t.Fatal("私号 chain_done 不该被自动下机")
	}
	if _, ok := st.Get("pv@x.com"); !ok {
		t.Fatal("私号状态行应保留（仅不下机）")
	}
	h.HandleEvent(clone("nm2@x.com"))
	if !st.IsRemoved("nm2@x.com") {
		t.Fatal("正常号 chain_done 应被自动下机（对照）")
	}

	// ③ hello（机器人重启/重连）→ 重推私号名单
	h.HandleEvent(map[string]any{"type": "hello", "pid": 1, "robot_version": "t", "_zone": testsupport.TestZoneKey})
	if pushCalls == 0 {
		t.Fatal("hello 后应重推私号名单（机器人侧标记自愈）")
	}
}
