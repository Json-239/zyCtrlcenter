// 2026-09-29 重登停滞事故（A 侧）：机器人进程重启（hello）标记离线时清**运行时忙态**。
//
// 背景：hello 原先只清 Online/HS → 残留 State=FIGHT/游荡/抓鬼 enabled → 水位补号候选
// 被 `Busy: hasLive && Busy(r)`（服务侧 PickOnlineCandidates 跳过 c.Busy）全部滤掉，
// 池空无人补号。A 的分界 = **只清运行时忙态**：满额标记/意图/台账/进度等不属 Robot 行，
// 本测试钉住 Robot 行内的保留集合。
package state

import (
	"testing"
)

func TestClearRuntimeBusyOnRestart(t *testing.T) {
	r := Robot{
		Account: "a@x.com", Online: true, HS: true,
		State: "FIGHT", Fight: true,
		Walk:  map[string]any{"enabled": true, "mapid": 10},
		Hatch: map[string]any{"active": true, "kind": "egg"},
		Ghost: map[string]any{"enabled": true, "done": 37, "limit": 50,
			"count_date": "20260929", "state": "FIGHT"},
		Booth: map[string]any{"enabled": true, "state": "OPEN"},
		// 以下字段必须保留（数据类，不属"运行时忙态"）：
		Level: 45, ChainDone: true, TaskIndex: 2019511, Done: 12,
		DoubleClaimDate: "20260929", Money: 123456, Reserve: 777,
		StuckCount: 2, StuckDay: "20260929", ErrRepeat: 1, ErrCode: "TASK_STUCK",
		Team: map[string]any{"role": "member", "captain_role_id": 501},
	}
	r.ClearRuntimeBusyOnRestart()

	// 清掉的部分
	if r.Online || r.HS {
		t.Fatal("Online/HS 应清掉")
	}
	if r.State != "OFFLINE" || r.Fight {
		t.Fatalf("State 应=OFFLINE 且 Fight=false，实际 %q/%v", r.State, r.Fight)
	}
	if r.Walk != nil || r.Hatch != nil {
		t.Fatal("Walk/Hatch 应清掉（否则 Walking()=true 仍算忙）")
	}
	if r.Booth != nil {
		t.Fatal("Booth 应清掉（重启=收摊，防面板残留'摆摊中'）")
	}
	if r.GhostActive() {
		t.Fatal("Ghost.enabled 应=false（GhostActive 清掉）")
	}
	// 抓鬼进度展示信息保留
	if r.Ghost["done"] != 37 || r.Ghost["limit"] != 50 || r.Ghost["count_date"] != "20260929" {
		t.Fatalf("Ghost 的 done/limit/count_date 应保留（进度展示），实际 %v", r.Ghost)
	}
	// 保留的部分（反例：这些被清掉就是越界）
	if r.Level != 45 || !r.ChainDone || r.TaskIndex != 2019511 || r.Done != 12 {
		t.Fatal("等级/链完成/任务索引/done 属数据类，不该被清")
	}
	if r.DoubleClaimDate != "20260929" || r.Money != 123456 || r.Reserve != 777 {
		t.Fatal("领双日期/货币不该被清")
	}
	if r.StuckCount != 2 || r.StuckDay != "20260929" || r.ErrRepeat != 1 || r.ErrCode != "TASK_STUCK" {
		t.Fatal("卡死计数/熔断相关（跨行存续口径）不该被清")
	}
	if r.TeamRole() != "member" {
		t.Fatal("组队心跳块不该被清（等下次心跳自然刷）")
	}
	// 幂等：重复调用不 panic、结果稳定
	r.ClearRuntimeBusyOnRestart()
	if r.Online || r.Walk != nil || r.GhostActive() {
		t.Fatal("重复调用结果应稳定")
	}
}
