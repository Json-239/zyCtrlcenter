// 2026-09-29 P0-5：恢复引擎"跳过"要留痕 —— Skip 闸 / needsRestore=false 原来全静默，
// 现场 robot0005054 空转 8 小时从日志上无法定位（"没人救"与"不需要救"分不出）。
//
// 口径：按"账号|意图"节流，每 10 分钟至多一条 [RESTORE-SKIP]；同号换意图立刻给新条
// （跟踪意图迁移）；Skip 原因与 state 都写进行内，便于 grep。
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

type skipLogFake struct {
	now    time.Time
	items  []intent.Intent
	robots map[string]state.Robot
	skip   map[string]string // account → Skip 原因（不在表里 = 不跳过）
	logs   []string
}

func newSkipLogFake() *skipLogFake {
	return &skipLogFake{
		now:    time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		robots: map[string]state.Robot{},
		skip:   map[string]string{},
	}
}

func (f *skipLogFake) deps() restorer.Deps {
	return restorer.Deps{
		Enabled:   func() bool { return true },
		ChannelUp: func() bool { return true },
		Intents:   func() []intent.Intent { return f.items },
		Robot:     func(a string) (state.Robot, bool) { r, ok := f.robots[a]; return r, ok },
		Now:       func() time.Time { return f.now },
		Skip: func(kind, account string) (bool, string) {
			why, ok := f.skip[account]
			return ok, why
		},
		Log: func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	}
}

func skipLogJoin(logs []string) string { return strings.Join(logs, "\n") }

// Skip 闸跳过：逐号节流（10 分钟内一条），窗口过后补一条；行内含账号/意图/原因。
func TestRestoreSkipLogThrottledPerAccountKind(t *testing.T) {
	f := newSkipLogFake()
	f.items = []intent.Intent{{Account: "sb1@x.com", Kind: intent.KindShenbu}}
	f.robots["sb1@x.com"] = state.Robot{Account: "sb1@x.com", Online: true, State: "IDLE"}
	f.skip["sb1@x.com"] = "今日大唐神捕已满/不可用（等跨日）"

	r := restorer.New(f.deps())
	r.Tick(f.now)
	if len(f.logs) != 1 {
		t.Fatalf("Skip 命中应留一条 [RESTORE-SKIP]，实际 %v", f.logs)
	}
	line := f.logs[0]
	for _, want := range []string{"[RESTORE-SKIP]", "sb1@x.com", "shenbu", "已满"} {
		if !strings.Contains(line, want) {
			t.Fatalf("跳过日志应含 %q，实际 %q", want, line)
		}
	}

	// 5 分钟后仍在窗口内：不重复
	f.now = f.now.Add(5 * time.Minute)
	r.Tick(f.now)
	if len(f.logs) != 1 {
		t.Fatalf("节流窗口内不该重复刷日志，实际 %v", f.logs)
	}

	// 超过 10 分钟：补一条（长时跳过可被"持续看到"，这是 P0-5 的排障目标）
	f.now = f.now.Add(6 * time.Minute)
	r.Tick(f.now)
	if len(f.logs) != 2 {
		t.Fatalf("窗口过后应补一条，实际 %v", f.logs)
	}
}

// needsRestore=false（在跑/游荡/冻结态）同样要留痕，并把 state 写出来（5054 是 state=DIALOG
// + 日常意图的组合，旧逻辑连一行都没有）。
func TestRestoreSkipLogForBusyAndWalkingStates(t *testing.T) {
	f := newSkipLogFake()
	f.items = []intent.Intent{
		{Account: "nav1@x.com", Kind: intent.KindFenghuo},
		{Account: "walk1@x.com", Kind: intent.KindGhost},
		{Account: "dlg1@x.com", Kind: intent.KindFenghuo},
	}
	f.robots["nav1@x.com"] = state.Robot{Account: "nav1@x.com", Online: true, State: "NAV"}
	w := state.Robot{Account: "walk1@x.com", Online: true, State: "IDLE"}
	w.Walk = map[string]any{"enabled": true}
	f.robots["walk1@x.com"] = w
	// DIALOG + 日常意图：needsRestore 只对 ghost 意图放行 —— 这就是 5054 的形态，必须留痕
	f.robots["dlg1@x.com"] = state.Robot{Account: "dlg1@x.com", Online: true, State: "DIALOG"}

	r := restorer.New(f.deps())
	r.Tick(f.now)
	joined := skipLogJoin(f.logs)
	for _, want := range []string{
		"nav1@x.com", "state=NAV",
		"walk1@x.com", "游荡中",
		"dlg1@x.com", "state=DIALOG",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("跳过日志应含 %q，实际:\n%s", want, joined)
		}
	}
	if len(f.logs) != 3 {
		t.Fatalf("三个号各一条（不同号不互相节流），实际 %d 条:\n%s", len(f.logs), joined)
	}

	// 同号换意图（fenghuo→shenbu）：节流键含 kind → 立刻给新条（跟踪意图迁移）
	f.items[0] = intent.Intent{Account: "nav1@x.com", Kind: intent.KindShenbu}
	f.now = f.now.Add(time.Minute)
	r.Tick(f.now)
	if len(f.logs) != 4 || !strings.Contains(f.logs[3], "shenbu") {
		t.Fatalf("换意图应立即补一条（键含 kind），实际:\n%s", skipLogJoin(f.logs))
	}
}

// 不跳过（正常补发）时不该有 [RESTORE-SKIP] 噪声。
func TestRestoreSkipLogAbsentWhenDispatching(t *testing.T) {
	f := newSkipLogFake()
	f.items = []intent.Intent{{Account: "ok1@x.com", Kind: intent.KindGhost}}
	f.robots["ok1@x.com"] = state.Robot{Account: "ok1@x.com", Online: true, State: "IDLE"}

	r := restorer.New(f.deps())
	acts := r.Tick(f.now)
	if len(acts) != 1 {
		t.Fatalf("空闲号应按意图补发，实际 %+v", acts)
	}
	for _, l := range f.logs {
		if strings.Contains(l, "RESTORE-SKIP") {
			t.Fatalf("正常补发不该有跳过日志，实际 %v", f.logs)
		}
	}
}
