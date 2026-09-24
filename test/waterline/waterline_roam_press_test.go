// 2026-09-23 水位压号修复：**纯游荡号可以直接压**（压前先停游荡）。
//
// 现场：在线 243 超出水位目标 230，压号一直"待下线 N 个，等它们收工"却永远不动 ——
// Busy() 把 Walking（游荡）也算忙 → 游荡号只能进待下线，而**游荡不会收工** → 死等。
// 修复：压号判定用 Pressable（比 Busy 宽一档，只多"纯游荡"这一类），并新增
// Deps.StopRoam —— 压前先停游荡再断（与游荡池回收含孵化号先停工同口径）。
package waterline_test

import (
	"path/filepath"
	"testing"

	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
)

// roaming 一个"纯游荡"号（Walk enabled，无任务/抓鬼/战斗）。
func roaming(acc string) state.Robot {
	return state.Robot{Account: acc, Online: true, State: "IDLE", HS: true,
		Walk: map[string]any{"enabled": true}}
}

// Pressable：游荡可直接压；任务推进/抓鬼会话/战斗/异常/暂停不可压。
func TestPressableJudgement(t *testing.T) {
	cases := []struct {
		name string
		r    state.Robot
		want bool
	}{
		{"空闲（不游荡，2026-09-24 起不再压）", online("a"), false},
		{"纯游荡", roaming("b"), true},
		{"抓鬼会话", ghosting("c"), false},
		{"任务推进 NAV", state.Robot{Account: "d", Online: true, State: "NAV"}, false},
		{"交付中 SUBMIT", state.Robot{Account: "e", Online: true, State: "SUBMIT"}, false},
		{"卡住 ERROR", state.Robot{Account: "f", Online: true, State: "ERROR"}, false},
		{"等鬼 WAIT_GHOST", state.Robot{Account: "g", Online: true, State: "WAIT_GHOST"}, false},
		{"战斗中", state.Robot{Account: "h", Online: true, State: "FIGHT", Fight: true}, false},
		{"人工暂停", state.Robot{Account: "i", Online: true, State: "IDLE", Paused: true}, false},
	}
	for _, c := range cases {
		if got := waterline.Pressable(c.r); got != c.want {
			t.Fatalf("%s: Pressable=%v want=%v", c.name, got, c.want)
		}
	}
}

// PickOffline：纯游荡号直接进 now；空闲号**既不压也不等**（2026-09-24 用户口径：只压在游荡的）。
func TestPickOfflineRoamingGoesNowDirectly(t *testing.T) {
	robots := []state.Robot{roaming("roam1"), online("idle1"), ghosting("busy1")}
	now, pending := waterline.PickOffline(robots, 3, true)
	if len(now) != 1 || now[0] != "roam1" {
		t.Fatalf("只有游荡号可立刻压: now=%v", now)
	}
	if len(pending) != 1 || pending[0] != "busy1" {
		t.Fatalf("抓鬼中的号仍只进待下线: pending=%v", pending)
	}
	if len(now)+len(pending) != 2 {
		t.Fatalf("空闲号不参与（不压不等）: now=%v pending=%v", now, pending)
	}
}

// Tick：超水位 + 游荡号 → 先停游荡、再下发下线（现场 243>230 修复的端到端证据）。
func TestTickPressRoamingStopsRoamFirst(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.robots = []state.Robot{roaming("r1"), roaming("r2"), online("i1")}
	f.local = 3
	f.svr, f.svrOK, f.svrTSms = 400, true, float64(f.now.UnixMilli()) // 远超目标 → 压号
	k := waterline.New(filepath.Join(dir, "waterline.json"), f.deps())
	cfg := waterline.DefaultConfig()
	cfg.Enabled = true
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if acted := k.Tick(f.now); !acted {
		t.Fatal("超水位应触发压号")
	}
	if len(f.stopRoamCalls) != 1 || len(f.stopRoamCalls[0]) != 2 {
		t.Fatalf("压号前应先对 2 个游荡号停游荡: %v", f.stopRoamCalls)
	}
	if len(f.offlineCalls) != 1 || len(f.offlineCalls[0]) != 2 {
		t.Fatalf("2 个游荡号下发下线（空闲号不压）: %v", f.offlineCalls)
	}
	if st := k.Status(); len(st.Pending) != 0 {
		t.Fatalf("没有忙号 → 不该有待下线（空闲号也不进 pending）: %+v", st.Pending)
	}
}
