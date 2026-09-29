// ④ 2026-09-29（神捕闸门修复④）：恢复引擎"离线已派号定向补拉"。
//
// 现状：restorer 对离线号静默 continue —— "已派神捕但掉线"的号没有其他拥有人
// （水位只在低于目标时拉、池候选/补拉够不着），重启后永远无人补拉（现场 5165/5167/5176/5240）。
// 口径：日常玩法（shenbu/fenghuo）∧ 离线 ∧ 未暂停 → 调 Deps.Online 定向补拉（按号 10 分钟冷却）；
// 每个跳过分支至少落一条 [RESTORE-SKIP] 留痕。
package recover_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/restorer"
	"zyctrlcenter/internal/state"
)

type onlinePullFake struct {
	now    time.Time
	items  []intent.Intent
	robots map[string]state.Robot
	calls  [][]string // Online 每次调用的入参
	fail   bool       // true = Online 返回错误
	logs   []string
	noHook bool // true = 不装配 Online（老行为）
}

func newOnlinePullFake() *onlinePullFake {
	return &onlinePullFake{
		now:    time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		robots: map[string]state.Robot{},
	}
}

func (f *onlinePullFake) deps() restorer.Deps {
	d := restorer.Deps{
		Enabled:   func() bool { return true },
		ChannelUp: func() bool { return true },
		Intents:   func() []intent.Intent { return f.items },
		Robot:     func(a string) (state.Robot, bool) { r, ok := f.robots[a]; return r, ok },
		Now:       func() time.Time { return f.now },
		Log:       func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	}
	if !f.noHook {
		d.Online = func(accs []string) (int, error) {
			cp := append([]string{}, accs...)
			f.calls = append(f.calls, cp)
			if f.fail {
				return 0, fmt.Errorf("通道未连接")
			}
			return len(accs), nil
		}
	}
	return d
}

func TestRestoreOnlinePullForOfflineDailyAssigned(t *testing.T) {
	f := newOnlinePullFake()
	f.items = []intent.Intent{{Account: "sb1@x.com", Kind: intent.KindShenbu}}
	f.robots["sb1@x.com"] = state.Robot{Account: "sb1@x.com", Online: false, State: "OFFLINE"}
	r := restorer.New(f.deps())

	r.Tick(f.now)
	if len(f.calls) != 1 || len(f.calls[0]) != 1 || f.calls[0][0] != "sb1@x.com" {
		t.Fatalf("离线已派神捕号应被定向补拉一次，实际 %v", f.calls)
	}
	if !strings.Contains(skipLogJoin(f.logs), "已定向补拉") {
		t.Fatalf("应留一条补拉留痕，实际 %v", f.logs)
	}

	// 冷却：10 分钟内再 Tick 不重复补拉（但可留"冷却中"低节流日志）
	f.logs = nil
	f.now = f.now.Add(30 * time.Second)
	r.Tick(f.now)
	if len(f.calls) != 1 {
		t.Fatalf("冷却期内不该重复补拉，实际 %v", f.calls)
	}
	// 冷却过后允许再补一次（号仍未上线）
	f.now = f.now.Add(601 * time.Second)
	r.Tick(f.now)
	if len(f.calls) != 2 {
		t.Fatalf("冷却过后应可再补拉，实际 %v", f.calls)
	}
}

func TestRestoreOnlinePullSkipsPausedAndNonDaily(t *testing.T) {
	f := newOnlinePullFake()
	f.items = []intent.Intent{
		{Account: "paused@x.com", Kind: intent.KindShenbu},
		{Account: "ghost@x.com", Kind: intent.KindGhost},
	}
	f.robots["paused@x.com"] = state.Robot{Account: "paused@x.com", Online: false, Paused: true}
	f.robots["ghost@x.com"] = state.Robot{Account: "ghost@x.com", Online: false}
	r := restorer.New(f.deps())
	r.Tick(f.now)
	if len(f.calls) != 0 {
		t.Fatalf("暂停号/非日常玩法不该被定向补拉，实际 %v", f.calls)
	}
	joined := skipLogJoin(f.logs)
	if !strings.Contains(joined, "paused@x.com") || !strings.Contains(joined, "人工暂停") {
		t.Fatalf("暂停号应留痕（不静默），实际 %v", f.logs)
	}
	if !strings.Contains(joined, "ghost@x.com") || !strings.Contains(joined, "池子/水位") {
		t.Fatalf("非日常离线号应留痕说明归属，实际 %v", f.logs)
	}
}

func TestRestoreOnlinePullFailureLogs(t *testing.T) {
	f := newOnlinePullFake()
	f.fail = true
	f.items = []intent.Intent{{Account: "sb2@x.com", Kind: intent.KindFenghuo}}
	f.robots["sb2@x.com"] = state.Robot{Account: "sb2@x.com", Online: false}
	r := restorer.New(f.deps())
	r.Tick(f.now)
	if len(f.calls) != 1 {
		t.Fatalf("应尝试一次补拉: %v", f.calls)
	}
	if !strings.Contains(skipLogJoin(f.logs), "定向补拉失败") {
		t.Fatalf("失败应留痕（含原因），实际 %v", f.logs)
	}
}

func TestRestoreOnlinePullNoHookKeepsLegacy(t *testing.T) {
	f := newOnlinePullFake()
	f.noHook = true
	f.items = []intent.Intent{{Account: "sb3@x.com", Kind: intent.KindShenbu}}
	f.robots["sb3@x.com"] = state.Robot{Account: "sb3@x.com", Online: false}
	r := restorer.New(f.deps())
	r.Tick(f.now) // 不 panic、不补拉（旧行为）
	if !strings.Contains(skipLogJoin(f.logs), "未装配自动补拉") {
		t.Fatalf("未装配也应留痕说明，实际 %v", f.logs)
	}
}
