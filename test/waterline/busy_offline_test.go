// 2026-09-29 重登停滞事故 B 项：`Busy` 只在**在线号**上成立 ——
// 离线号不存在"被打扰"，反而是补号候选；残留忙态（Hello 未清/崩溃/断链遗留）
// 不得把离线号从补号里滤掉（PickOnlineCandidates 会跳过 c.Busy）。
package waterline_test

import (
	"testing"

	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
)

func TestBusyIgnoresOffline(t *testing.T) {
	cases := []state.Robot{
		{Account: "a", Online: false, State: "FIGHT"},
		{Account: "a", Online: false, State: "NAV"},
		{Account: "a", Online: false, Fight: true},
		{Account: "a", Online: false, Walk: map[string]any{"enabled": true}},
		{Account: "a", Online: false, Ghost: map[string]any{"enabled": true}},
		{Account: "a", Online: false, State: "WAIT_TASK", TaskIndex: 3},
	}
	for i, r := range cases {
		if waterline.Busy(r) {
			t.Fatalf("case %d：离线号的忙态应一律不算（否则会被补号候选跳过）: %+v", i, r)
		}
	}
	// 反例：同一批字段在线时仍然是"忙"（B 只豁免离线，不改在线语义）
	if !waterline.Busy(state.Robot{Account: "a", Online: true, State: "FIGHT"}) {
		t.Fatal("在线 + FIGHT 仍应算忙")
	}
	if !waterline.Busy(state.Robot{Account: "a", Online: true, Fight: true}) {
		t.Fatal("在线 + Fight 仍应算忙")
	}
}
